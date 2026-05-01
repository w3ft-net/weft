package compute

import (
	"context"
	"strings"
)

// Line is one row of output from a compute job. Mirrors what the user
// would see in their shell — a rendered line with optional metadata
// for callers that need it.
type Line struct {
	Raw     string
	LineNum int64 // 0 when not shard-sourced or post-stage
}

// LineStream is a forward-only stream of compute job output. Next
// returns ok=false when the stream is exhausted; Close releases any
// underlying resources (idempotent).
type LineStream interface {
	Next(ctx context.Context) (Line, bool, error)
	Close() error
}

// bufferedLineStream adapts a "produce the whole text" function into a
// LineStream. The full text is materialized on the first Next call and
// then walked line-by-line. This is the bridge implementation used
// while ComputeJob still computes results as a single blob; a future
// pass replaces it with a producer-side line channel for true
// incremental delivery.
type bufferedLineStream struct {
	produce func() (string, error)
	lines   []string
	idx     int
	init    bool
}

func newBufferedLineStream(produce func() (string, error)) *bufferedLineStream {
	return &bufferedLineStream{produce: produce}
}

func (s *bufferedLineStream) Next(_ context.Context) (Line, bool, error) {
	if !s.init {
		text, err := s.produce()
		s.init = true
		if err != nil {
			return Line{}, false, err
		}
		if text != "" {
			s.lines = strings.Split(strings.TrimRight(text, "\n"), "\n")
		}
	}
	if s.idx >= len(s.lines) {
		return Line{}, false, nil
	}
	line := Line{Raw: s.lines[s.idx]}
	s.idx++
	return line, true, nil
}

func (s *bufferedLineStream) Close() error { return nil }
