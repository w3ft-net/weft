package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/w3ft-net/weft/backends/logs/live"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// Volume is the shard's data volume as the process sees it: Mount blocks
// until the volume is attached to this instance and mounted at the data
// directory (formatting a blank one), Unmount releases it so the leader
// can detach it. Both are idempotent. See docs/plans/
// shard-seamless-upgrade.md.
type Volume interface {
	Mount(ctx context.Context) error
	Unmount(ctx context.Context) error
}

// serveMode is the gate every data call passes through.
type serveMode int

const (
	// modeServing admits calls and counts them in flight.
	modeServing serveMode = iota
	// modeNotReady refuses with a not-ready status: a spare with no volume,
	// or a shard remounting its volume on Resume.
	modeNotReady
	// modePaused refuses with a paused status naming the successor.
	modePaused
)

// pauseDrainPoll is how often Pause rechecks the in-flight count.
const pauseDrainPoll = 5 * time.Millisecond

// pauseQueryGrace is how long Pause lets admitted queries run before it
// cuts them, so one long query can't hold every caller past their hold
// timeout. Crons get 2s a run, so this never cuts one short; ad hoc
// queries may run up to ShardQueryTimeoutSeconds (30s). A query cut
// before its first row is refused as paused and re-runs on the successor;
// one already streaming fails, since its rows so far are incomplete.
// Writes are never cut: a cancelled write may already be applied, and its
// retry would duplicate it. A var for tests.
var pauseQueryGrace = 2 * time.Second

// errPauseCut is the cancel cause of a query Pause cut.
var errPauseCut = errors.New("shard paused")

// gatedQuery is an admitted Query stream: its context is the one Pause
// cancels, and it notes whether a row has gone out.
type gatedQuery struct {
	grpc.ServerStream
	ctx    context.Context
	cancel context.CancelCauseFunc
	sent   atomic.Bool
}

func (q *gatedQuery) Context() context.Context { return q.ctx }

func (q *gatedQuery) SendMsg(m any) error {
	q.sent.Store(true)
	return q.ServerStream.SendMsg(m)
}

// gateExempt are the calls the gate never refuses: the leader drives the
// pause through them.
var gateExempt = map[string]bool{
	pb.ShardQuery_Pause_FullMethodName:  true,
	pb.ShardQuery_Resume_FullMethodName: true,
}

// admit lets a call in (counting it in flight, and registering q, a
// query, as one Pause may cut) or returns the refusal. The mode check and
// the count happen under one lock, so a call admitted before Pause flips
// the mode is one Pause waits for, and none is admitted after.
func (s *Server) admit(method string, q *gatedQuery) (release func(), err error) {
	if gateExempt[method] {
		return func() {}, nil
	}
	s.gateMu.Lock()
	defer s.gateMu.Unlock()
	switch s.mode {
	case modePaused:
		return nil, shard.PausedError(s.successor)
	case modeNotReady:
		return nil, shard.NotReadyError()
	}
	s.inflight++
	if q != nil {
		if s.queries == nil {
			s.queries = map[*gatedQuery]struct{}{}
		}
		s.queries[q] = struct{}{}
	}
	return func() {
		s.gateMu.Lock()
		s.inflight--
		delete(s.queries, q)
		s.gateMu.Unlock()
	}, nil
}

// UnaryInterceptor gates every unary shard call (see admit). It must sit
// after auth in the chain so an unauthenticated caller learns nothing.
func (s *Server) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		release, err := s.admit(info.FullMethod, nil)
		if err != nil {
			return nil, err
		}
		defer release()
		return handler(ctx, req)
	}
}

// StreamInterceptor is UnaryInterceptor for streams. A refused Query fails
// before it sends a row, so a retry can't double-send. A Query that Pause
// cuts (see pauseQueryGrace) is refused as paused if it hadn't sent a row,
// so the caller re-runs it on the successor, and fails otherwise.
func (s *Server) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if info.FullMethod != pb.ShardQuery_Query_FullMethodName {
			release, err := s.admit(info.FullMethod, nil)
			if err != nil {
				return err
			}
			defer release()
			return handler(srv, ss)
		}
		ctx, cancel := context.WithCancelCause(ss.Context())
		defer cancel(nil)
		q := &gatedQuery{ServerStream: ss, ctx: ctx, cancel: cancel}
		release, err := s.admit(info.FullMethod, q)
		if err != nil {
			return err
		}
		defer release()
		err = handler(srv, q)
		if err == nil || !errors.Is(context.Cause(ctx), errPauseCut) {
			return err
		}
		if q.sent.Load() {
			return status.Error(codes.Unavailable, "shard paused mid-query; the rows it sent are incomplete")
		}
		s.gateMu.Lock()
		successor := s.successor
		s.gateMu.Unlock()
		return shard.PausedError(successor)
	}
}

// cutQueries cancels every admitted query and returns how many.
func (s *Server) cutQueries() int {
	s.gateMu.Lock()
	defer s.gateMu.Unlock()
	for q := range s.queries {
		q.cancel(errPauseCut)
	}
	return len(s.queries)
}

// Spare reports whether this instance is a warm-pool spare that has no
// volume yet. A spare heartbeats as one and is never a shard.
func (s *Server) Spare() bool {
	s.gateMu.Lock()
	defer s.gateMu.Unlock()
	return s.spare
}

// Paused reports whether the shard is refusing calls with a redirect. A
// paused shard stops heartbeating: its successor carries the shard now, and
// a beat from the old address would pull callers back to it.
func (s *Server) Paused() bool {
	s.gateMu.Lock()
	defer s.gateMu.Unlock()
	return s.mode == modePaused
}

// NewSpareServer is a warm-pool instance: no volume, no database, every
// data call refused as not ready. Activate turns it into a shard once the
// leader attaches a volume.
func NewSpareServer(dataDir, dbPath string, vol Volume, opts ...Option) *Server {
	s := newServer(opts)
	s.dataDir, s.dbPath, s.Volume = dataDir, dbPath, vol
	s.mode, s.spare, s.released = modeNotReady, true, true
	return s
}

// Activate waits for the volume to be attached, mounts it, and opens
// whatever is on it: the shard whose identity it carries (an upgrade's or
// recovery's volume), or nothing (a fresh volume for a new shard, which
// then waits for Configure). Only then are calls admitted.
func (s *Server) Activate(ctx context.Context) error {
	if err := s.Volume.Mount(ctx); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	opening := time.Now()
	if err := s.openFromDisk(); err != nil {
		return err
	}
	s.gateMu.Lock()
	s.released = false
	s.spare = false
	s.mode = modeServing
	s.gateMu.Unlock()
	timing := ""
	s.mu.RLock()
	if s.shard != nil { // a blank volume carries no shard to open
		timing = " (" + s.shard.OpenTiming() + ")"
	}
	s.mu.RUnlock()
	weftsdk.Infof(ctx, "spare activated as %q; opening its database took %s%s", s.Identity(), time.Since(opening), timing)
	return nil
}

// openFromDisk opens the shard identified by shard.json in the data
// directory, or leaves the server blank when there is none.
func (s *Server) openFromDisk() error {
	id, err := shard.LoadIdentity(s.dataDir)
	if err != nil {
		return fmt.Errorf("load identity: %w", err)
	}
	if id == nil {
		return nil
	}
	ds, err := s.open(id.ShardID, s.dbPath, id.Layout)
	if err != nil {
		return fmt.Errorf("open %s store at %s: %w", s.driver.Name(), s.dbPath, err)
	}
	s.mu.Lock()
	s.shard = ds
	s.id = id.ShardID
	s.apps = id.Apps
	s.heartbeatToken = id.HeartbeatToken
	s.mu.Unlock()
	return nil
}

// Pause refuses every new call with a redirect to req.SuccessorAddr, waits
// for the calls already admitted (cutting queries that outlive
// pauseQueryGrace), then checkpoints and closes DuckDB and
// unmounts the volume, so the leader can move it. Idempotent: a repeat
// only updates the successor. If the drain outlives ctx the shard stays
// paused with its database open; the leader's Resume brings it back.
func (s *Server) Pause(ctx context.Context, req *pb.PauseRequest) (*pb.PauseResponse, error) {
	s.gateMu.Lock()
	if s.mode == modeNotReady {
		s.gateMu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "shard is not serving; nothing to pause")
	}
	s.mode = modePaused
	s.successor = req.SuccessorAddr
	s.gateMu.Unlock()

	start := time.Now()
	if err := inSpan(ctx, "pause.drain", func(ctx context.Context) error {
		graceCtx, cancelGrace := context.WithTimeout(ctx, pauseQueryGrace)
		err := s.waitDrained(graceCtx)
		cancelGrace()
		if err == nil || ctx.Err() != nil {
			return err
		}
		if n := s.cutQueries(); n > 0 {
			weftsdk.Infof(ctx, "shard %s: pause cut %d queries still running after %s", s.Identity(), n, pauseQueryGrace)
		}
		return s.waitDrained(ctx)
	}); err != nil {
		return nil, err
	}
	drained := time.Now()

	if ds := s.Shard(); ds != nil {
		if err := inSpan(ctx, "pause.checkpoint", func(ctx context.Context) error {
			// Ingest has stopped: let the classifier finish, so the facts
			// it kept on the volume are current when the successor opens it.
			if f, ok := ds.(live.FieldFactSource); ok {
				f.FlushFieldFacts()
			}
			if err := ds.Checkpoint(ctx); err != nil {
				return fmt.Errorf("checkpoint: %w", err)
			}
			if err := ds.Close(); err != nil {
				return fmt.Errorf("close store: %w", err)
			}
			return nil
		}); err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.shard = nil
		s.mu.Unlock()
	}
	closed := time.Now()

	s.gateMu.Lock()
	released := s.released
	s.gateMu.Unlock()
	if !released && s.Volume != nil {
		if err := inSpan(ctx, "pause.unmount", s.Volume.Unmount); err != nil {
			return nil, fmt.Errorf("unmount: %w", err)
		}
		s.gateMu.Lock()
		s.released = true
		s.gateMu.Unlock()
	}
	weftsdk.Infof(ctx, "shard %s: paused and released its volume in %s (drain %s, checkpoint and close %s, unmount %s); redirecting to %s",
		s.Identity(), time.Since(start), drained.Sub(start), closed.Sub(drained), time.Since(closed), req.SuccessorAddr)
	return &pb.PauseResponse{}, nil
}

// inSpan runs fn as the span name in ctx's trace, recording its error.
func inSpan(ctx context.Context, name string, fn func(context.Context) error) error {
	sctx, span := weftsdk.StartSpan(ctx, name)
	err := fn(sctx)
	span.RecordError(err)
	span.End()
	return err
}

func (s *Server) waitDrained(ctx context.Context) error {
	t := time.NewTicker(pauseDrainPoll)
	defer t.Stop()
	for {
		s.gateMu.Lock()
		n := s.inflight
		s.gateMu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("drain %d in-flight calls: %w", n, ctx.Err())
		case <-t.C:
		}
	}
}

// Resume undoes Pause: remount the volume if it was released, reopen
// DuckDB if it was closed, and admit calls. Calls are refused as not ready
// (callers hold) while the volume comes back. A no-op when serving.
func (s *Server) Resume(ctx context.Context, _ *pb.ResumeRequest) (*pb.ResumeResponse, error) {
	s.gateMu.Lock()
	switch {
	case s.mode == modeServing:
		s.gateMu.Unlock()
		return &pb.ResumeResponse{}, nil
	case s.spare:
		s.gateMu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "a spare has nothing to resume")
	}
	s.mode = modeNotReady
	released := s.released
	s.gateMu.Unlock()
	start := time.Now()

	if released && s.Volume != nil {
		if err := s.Volume.Mount(ctx); err != nil {
			return nil, fmt.Errorf("remount: %w", err)
		}
		s.gateMu.Lock()
		s.released = false
		s.gateMu.Unlock()
	}
	if s.Shard() == nil {
		if err := s.openFromDisk(); err != nil {
			return nil, err
		}
	}
	s.gateMu.Lock()
	s.mode = modeServing
	s.successor = ""
	s.gateMu.Unlock()
	weftsdk.Infof(ctx, "shard %s: resumed in %s", s.Identity(), time.Since(start))
	return &pb.ResumeResponse{}, nil
}
