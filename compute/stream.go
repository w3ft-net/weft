package compute

import (
	"context"
	"strings"
)

// Line is one row of output from a compute job. Mirrors what the user
// would see in their shell — a rendered line with optional metadata
// for callers that need it. Stderr=true tags alert/error output so
// clients can render distinctly (red ANSI, separate channel, etc.).
type Line struct {
	Raw     string
	LineNum int64 // 1-based sequential counter for zero-stage jobs; 0 for staged output
	Stderr  bool
}

// LineStream is a forward-only stream of compute job output. Next
// returns ok=false when the stream is exhausted; Close releases any
// underlying resources (idempotent).
type LineStream interface {
	Next(ctx context.Context) (Line, bool, error)
	Close() error
}

// channelLineStream wraps a chan Line so producers can push lines
// incrementally — used by Job.consume after Phase 4's
// producer-side streaming refactor. Errors propagate via the
// dedicated err pointer; producer sets it before closing the channel
// so consumers see the error after the channel signals exhaustion.
type channelLineStream struct {
	ch     <-chan Line
	errPtr *error // populated by producer before closing ch; nil = success
}

func newChannelLineStream(ch <-chan Line, errPtr *error) *channelLineStream {
	return &channelLineStream{ch: ch, errPtr: errPtr}
}

func (s *channelLineStream) Next(ctx context.Context) (Line, bool, error) {
	select {
	case <-ctx.Done():
		return Line{}, false, ctx.Err()
	case line, ok := <-s.ch:
		if !ok {
			if s.errPtr != nil && *s.errPtr != nil {
				return Line{}, false, *s.errPtr
			}
			return Line{}, false, nil
		}
		return line, true, nil
	}
}

func (s *channelLineStream) Close() error { return nil }

// sliceLineStream replays an in-memory []Line. Used as a buffered
// view over a previously-drained stream — Result() drains the
// channel into a slice, then subsequent Stream() calls replay from
// the slice so multiple readers can see the same output.
type sliceLineStream struct {
	lines []Line
	err   error
	idx   int
}

func newSliceLineStream(lines []Line, err error) *sliceLineStream {
	return &sliceLineStream{lines: lines, err: err}
}

// NewSliceLineStream is the exported constructor for callers outside
// compute that need a buffered LineStream — typically the coordinator
// when it has to stream a previously-drained body (alert post-
// processing, redirect to /tmp, etc.).
func NewSliceLineStream(lines []Line, err error) LineStream {
	return newSliceLineStream(lines, err)
}

func (s *sliceLineStream) Next(_ context.Context) (Line, bool, error) {
	if s.idx >= len(s.lines) {
		return Line{}, false, s.err
	}
	line := s.lines[s.idx]
	s.idx++
	return line, true, nil
}

func (s *sliceLineStream) Close() error { return nil }

// DrainLineStream reads s to exhaustion and returns the lines plus
// any terminal error.
func DrainLineStream(ctx context.Context, s LineStream) ([]Line, error) {
	return drainLineStream(ctx, s)
}

func drainLineStream(ctx context.Context, s LineStream) ([]Line, error) {
	var out []Line
	for {
		line, ok, err := s.Next(ctx)
		if err != nil {
			return out, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, line)
	}
}

// linesToText joins drained lines into a newline-terminated string.
func linesToText(lines []Line) string {
	if len(lines) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, line := range lines {
		sb.WriteString(line.Raw)
		sb.WriteByte('\n')
	}
	return sb.String()
}
