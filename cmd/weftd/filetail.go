package main

import (
	"context"
	weftsdk "github.com/w3ft-net/weft-go"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/weftd"
)

// File shipping with acked checkpoints (docs/plans/weftd-offset-checkpointing.md).
//
// IngestStream is client-streaming: the only response arrives at half-close,
// so a nil Send means "buffered locally", nothing more. To learn what the
// coordinator has, the shipper keeps sent-but-unacked rows in a bounded
// window and periodically half-closes the stream. A successful CloseAndRecv
// proves the window was ingested, and only then does the checkpoint advance
// to the end offset of its last line. A failed one keeps the window, and the
// next attempt resends all of it on a fresh stream (at-least-once).

const (
	// rowOverheadBytes approximates what a buffered *pb.LogRow costs on top
	// of its Raw text, so the window bound tracks memory, not just payload.
	rowOverheadBytes = 256

	maxCommitBackoff = 30 * time.Second
)

// shipperConfig is the delivery tuning shared by every file tailer.
type shipperConfig struct {
	commitInterval  time.Duration // how long rows may wait for their ack cycle
	windowBytes     int           // unacked window bound; a full window forces a cycle
	shutdownTimeout time.Duration // bound on the final drain at shutdown
}

// shipCfg and checkpoints are set once in main before any tailer starts.
// A nil checkpoints store runs the legacy path: one long-lived stream, no
// window, no ack cycling.
var (
	shipCfg = shipperConfig{
		commitInterval:  15 * time.Second,
		windowBytes:     8 << 20,
		shutdownTimeout: 5 * time.Second,
	}
	checkpoints *weftd.CheckpointStore
	tailers     = &tailerGroup{}
)

// tailerGroup lets main wait for every file tailer's final commit before it
// flushes the checkpoint store and exits.
type tailerGroup struct {
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

func (g *tailerGroup) begin() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.wg.Add(1)
	return true
}

func (g *tailerGroup) done() { g.wg.Done() }

// shutdown refuses new tailers and waits for running ones to finish.
func (g *tailerGroup) shutdown() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.wg.Wait()
}

// jitter returns d +/-10%, so shippers that started together (a fleet-wide
// self-update) do not cycle their streams in synchronized waves.
func jitter(d time.Duration) time.Duration {
	spread := d / 10
	if spread <= 0 {
		return d
	}
	return d - spread + time.Duration(rand.Int64N(int64(2*spread)+1))
}

type windowEntry struct {
	row  *pb.LogRow
	end  int64
	file weftd.FileID
}

type fileShipper struct {
	client   pb.WeftCoordinatorClient
	instance string
	app      string
	cfg      shipperConfig
	ckpt     *weftd.CheckpointHandle
	track    bool // keep a window and cycle streams (checkpointing on)

	// Streams are children of streamCtx, which deliberately does not die
	// with the agent's ctx: the shutdown drain needs them alive to collect
	// the final ack. cancelStreams is the hard stop.
	streamCtx     context.Context
	cancelStreams context.CancelFunc

	stream       pb.WeftCoordinator_IngestStreamClient
	streamCancel context.CancelFunc

	window      []windowEntry
	windowBytes int
	failing     bool // the last open/ack failed; retry on the backoff timer, not per line
}

func runFileTailer(ctx context.Context, client pb.WeftCoordinatorClient, instance, app, path string, startup bool) {
	if !tailers.begin() {
		return
	}
	defer tailers.done()

	h, start := checkpoints.Open(path, startup)
	tailer, err := weftd.NewTailer(path, start)
	if err != nil {
		weftsdk.Infof(ctx, "[%s] open %s: %v", app, path, err)
		return
	}
	defer tailer.Close()

	s := newFileShipper(ctx, client, instance, app, h, shipCfg)
	defer s.cancelStreams()
	s.run(ctx, tailer.Lines())
}

func newFileShipper(ctx context.Context, client pb.WeftCoordinatorClient, instance, app string, h *weftd.CheckpointHandle, cfg shipperConfig) *fileShipper {
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	return &fileShipper{
		client: client, instance: instance, app: app, cfg: cfg,
		ckpt: h, track: h != nil,
		streamCtx: sctx, cancelStreams: cancel,
	}
}

func (s *fileShipper) run(ctx context.Context, lines <-chan weftd.Line) {
	// After shutdown begins, streams get cfg.shutdownTimeout to deliver and
	// collect the final ack, then are cancelled so a hung coordinator (or a
	// Send blocked on flow control) cannot hold the exit.
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
		case <-finished:
			return
		}
		select {
		case <-time.After(s.cfg.shutdownTimeout):
			s.cancelStreams()
		case <-finished:
		}
	}()

	timer := time.NewTimer(time.Hour)
	timer.Stop()
	armed := false
	arm := func(d time.Duration) {
		if armed {
			timer.Stop()
		}
		timer.Reset(d)
		armed = true
	}
	defer timer.Stop()
	backoff := time.Second

	for {
		in := lines
		if s.full() {
			in = nil // backpressure: the file is the buffer
		}
		select {
		case <-ctx.Done():
			s.finish()
			return

		case l, ok := <-in:
			if !ok {
				s.finish()
				return
			}
			s.add(l)
			if !s.track {
				continue
			}
			if len(s.window) > 0 && !armed {
				arm(jitter(s.cfg.commitInterval))
			}
			if s.full() && !s.failing {
				if s.commitAndRearm(arm, &backoff, &armed) {
					continue
				}
			}

		case <-timer.C:
			armed = false
			s.commitAndRearm(arm, &backoff, &armed)
		}
	}
}

// commitAndRearm runs one ack cycle and schedules the next attempt: a
// jittered interval if rows remain, an exponential backoff after a failure.
// It reports whether the cycle succeeded.
func (s *fileShipper) commitAndRearm(arm func(time.Duration), backoff *time.Duration, armed *bool) bool {
	if err := s.commit(); err != nil {
		s.failing = true
		weftsdk.Errorf(context.Background(), "[%s] ack cycle failed, retrying in %v: %v", s.app, *backoff, err)
		arm(*backoff)
		if *backoff *= 2; *backoff > maxCommitBackoff {
			*backoff = maxCommitBackoff
		}
		return false
	}
	s.failing = false
	*backoff = time.Second
	if len(s.window) > 0 {
		arm(jitter(s.cfg.commitInterval))
	}
	return true
}

func (s *fileShipper) full() bool {
	return s.track && s.windowBytes >= s.cfg.windowBytes
}

// add sends one line, opening a stream on demand.
func (s *fileShipper) add(l weftd.Line) {
	text := strings.TrimSpace(l.Text)
	if text == "" {
		return // nothing to ship; the checkpoint catches up on the next real line
	}
	row := buildLogRow(text, s.instance, s.app, "")

	if s.track {
		s.window = append(s.window, windowEntry{row: row, end: l.End, file: l.File})
		s.windowBytes += len(row.Raw) + rowOverheadBytes
		if s.stream == nil {
			if s.failing {
				return // the backoff timer owns reconnects during an outage
			}
			if err := s.openAndReplay(); err != nil {
				s.failing = true
				weftsdk.Infof(context.Background(), "[%s] open stream: %v", s.app, err)
			}
			return // openAndReplay already sent this row with the rest of the window
		}
		if err := s.stream.Send(row); err != nil {
			weftsdk.Infof(context.Background(), "[%s] stream broken: %v", s.app, err)
			s.dropStream() // the window is intact; the next ack cycle resends it
		}
		return
	}

	// Legacy path: one long-lived stream, reconnect on error, no resend window.
	if s.stream != nil {
		if err := s.stream.Send(row); err == nil {
			return
		}
		weftsdk.Infof(context.Background(), "[%s] stream broken, reconnecting", s.app)
		s.dropStream()
	}
	stream, cancel, err := s.openStream()
	if err != nil {
		weftsdk.Infof(context.Background(), "[%s] open stream: %v", s.app, err)
		return
	}
	s.stream, s.streamCancel = stream, cancel
	if err := s.stream.Send(row); err != nil {
		weftsdk.Errorf(context.Background(), "[%s] send failed after reconnect: %v", s.app, err)
		s.dropStream()
	}
}

func (s *fileShipper) openStream() (pb.WeftCoordinator_IngestStreamClient, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(s.streamCtx)
	stream, err := s.client.IngestStream(ctx)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return stream, cancel, nil
}

// openAndReplay opens a fresh stream and resends the whole unacked window.
func (s *fileShipper) openAndReplay() error {
	stream, cancel, err := s.openStream()
	if err != nil {
		return err
	}
	for _, e := range s.window {
		if err := stream.Send(e.row); err != nil {
			cancel()
			return err
		}
	}
	s.stream, s.streamCancel = stream, cancel
	return nil
}

func (s *fileShipper) dropStream() {
	if s.streamCancel != nil {
		s.streamCancel()
	}
	s.stream, s.streamCancel = nil, nil
}

// commit half-closes the stream to collect the coordinator's ack, then
// advances the checkpoint and clears the window. On any failure the window
// is kept for the next attempt.
func (s *fileShipper) commit() error {
	if len(s.window) == 0 {
		return nil
	}
	if s.stream == nil {
		if err := s.openAndReplay(); err != nil {
			return err
		}
	}
	_, err := s.stream.CloseAndRecv()
	s.dropStream() // the stream is finished either way; a new one opens on the next line
	if err != nil {
		return err
	}
	last := s.window[len(s.window)-1]
	s.ckpt.Commit(last.file, last.end)
	clear(s.window)
	s.window, s.windowBytes = s.window[:0], 0
	return nil
}

// finish is the shutdown drain: deliver and ack what has been read. Lines
// still queued in the tailer were never read past the checkpoint, so the next
// start re-reads them from the file.
func (s *fileShipper) finish() {
	if s.track {
		if err := s.commit(); err != nil {
			weftsdk.Infof(context.Background(), "[%s] shutdown drain: %d rows left unacked, they will be re-read: %v", s.app, len(s.window), err)
		}
		return
	}
	if s.stream != nil {
		// Half-close so the coordinator's handler sees EOF and returns
		// cleanly instead of waiting out the idle reaper.
		s.stream.CloseAndRecv()
		s.dropStream()
	}
}
