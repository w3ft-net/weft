package coordinator

import (
	"context"

	"github.com/graybeardcorp/weft/compute"
)

// LiveStream is implemented by a compute.LineStream that can report
// whether it has moved past a historical replay phase into live
// delivery. The gRPC Execute handler type-asserts for this to decide
// when per-line flushing (vs. batching up to the response size cap)
// actually matters — checked as an optional interface, not added to
// compute.LineStream itself, since concatStream is the only
// implementation for which the distinction is meaningful.
type LiveStream interface {
	compute.LineStream
	InLivePhase() bool
}

// concatStream exhausts primary first, then transitions to the stream
// produced by next(). next() is invoked lazily — only when primary
// returns (_, false, nil); a primary error short-circuits without
// advancing. Used by ExecuteFollow to splice a historical batch into
// a live broker subscription behind a single LineStream surface.
type concatStream struct {
	primary  compute.LineStream
	next     func() compute.LineStream
	current  compute.LineStream
	advanced bool
}

func (s *concatStream) Next(ctx context.Context) (compute.Line, bool, error) {
	if !s.advanced {
		line, ok, err := s.primary.Next(ctx)
		if err != nil {
			return compute.Line{}, false, err
		}
		if ok {
			return line, true, nil
		}
		s.current = s.next()
		s.advanced = true
		if s.current == nil {
			return compute.Line{}, false, nil
		}
	}
	return s.current.Next(ctx)
}

// InLivePhase reports whether primary has exhausted and Next is now
// serving from the live stream produced by next(). Lets a caller (the
// gRPC Execute handler) flush eagerly once historical replay gives way
// to live tail rows, while still batching normally during the
// historical phase itself.
func (s *concatStream) InLivePhase() bool {
	return s.advanced
}

func (s *concatStream) Close() error {
	var err error
	if e := s.primary.Close(); e != nil {
		err = e
	}
	if s.current != nil {
		if e := s.current.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}
