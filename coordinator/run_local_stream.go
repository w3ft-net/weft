package coordinator

import (
	"bufio"
	"context"
	"io"
	"sync"
	"sync/atomic"

	"github.com/graybeardcorp/weft/compute"
)

// liveScriptStream is the LineStream returned by streaming runLocal.
// It scans stdout and stderr io.Pipe readers concurrently, emitting
// each line as a compute.Line (Stderr=true for stderr-side lines)
// onto a shared channel. The channel closes when both scanners have
// observed EOF — which happens when runLocal's goroutine closes the
// corresponding PipeWriters after warp's ExecScript returns.
//
// The terminal error (timeout, scanner I/O failure, runLocal-side
// error) is surfaced on the final Next call after the channel
// closes. Set via setRunErr from the runLocal goroutine BEFORE it
// closes the pipe writers, so the consumer's read of err past EOF
// happens-after the write.
type liveScriptStream struct {
	ch     chan compute.Line
	runErr atomic.Pointer[error]

	// once-protected close of ch so a panic during scanning doesn't
	// turn into a double-close on cleanup paths.
	closeOnce sync.Once
}

func newLiveScriptStream(stdoutR, stderrR io.Reader) *liveScriptStream {
	s := &liveScriptStream{
		ch: make(chan compute.Line, 256),
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go s.scan(stdoutR, false, &wg)
	go s.scan(stderrR, true, &wg)

	go func() {
		wg.Wait()
		s.closeOnce.Do(func() { close(s.ch) })
	}()

	return s
}

// scan reads lines off r and pushes them onto s.ch tagged with the
// stderr flag. Scanner errors are surfaced via setRunErr (winning
// thread keeps its error; subsequent ones are dropped, matching
// "first error wins" semantics).
func (s *liveScriptStream) scan(r io.Reader, stderr bool, wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	// Logs can have long records (we ingest raw lines verbatim); cap
	// the per-line buffer at 16 MiB to match the rest of the streaming
	// path's tolerance.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		s.ch <- compute.Line{Raw: scanner.Text(), Stderr: stderr}
	}
	if err := scanner.Err(); err != nil {
		s.setRunErr(err)
	}
}

// setRunErr records err as the stream's terminal error, first-write-
// wins. Safe to call from any goroutine; the consumer reads runErr
// only after observing channel close (which happens-after both
// scanners have exited and any runLocal-goroutine setRunErr call has
// run, by program-order through the pipe-close → scanner-EOF →
// wg.Done chain).
func (s *liveScriptStream) setRunErr(err error) {
	if err == nil {
		return
	}
	s.runErr.CompareAndSwap(nil, &err)
}

// Next implements compute.LineStream.
func (s *liveScriptStream) Next(ctx context.Context) (compute.Line, bool, error) {
	select {
	case <-ctx.Done():
		return compute.Line{}, false, ctx.Err()
	case line, ok := <-s.ch:
		if !ok {
			if e := s.runErr.Load(); e != nil {
				return compute.Line{}, false, *e
			}
			return compute.Line{}, false, nil
		}
		return line, true, nil
	}
}

// Close implements compute.LineStream. The underlying pipes are
// closed by runLocal's goroutine when warp finishes; nothing for the
// consumer to release here. Idempotent so callers can defer freely.
func (s *liveScriptStream) Close() error {
	return nil
}
