package coordinator

import (
	"context"

	"github.com/kevlarcon/weft/compute"
)

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
