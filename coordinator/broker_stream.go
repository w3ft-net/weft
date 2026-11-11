package coordinator

import (
	"context"

	"github.com/graybeardcorp/weft/compute"
)

// brokerStream wraps a TailBroker subscription as a LineStream. Each
// arriving row passes through filter; rows that produce no lines are
// skipped; multi-line filter output is queued and emitted across
// consecutive Next calls. Close releases the broker subscription.
type brokerStream struct {
	ch     <-chan Row
	filter LiveFilter
	unsub  func()
	queue  []compute.Line
}

func (s *brokerStream) Next(ctx context.Context) (compute.Line, bool, error) {
	if len(s.queue) > 0 {
		line := s.queue[0]
		s.queue = s.queue[1:]
		return line, true, nil
	}
	for {
		select {
		case <-ctx.Done():
			return compute.Line{}, false, ctx.Err()
		case row, ok := <-s.ch:
			if !ok {
				return compute.Line{}, false, nil
			}
			lines := s.filter(row)
			if len(lines) == 0 {
				continue
			}
			s.queue = s.queue[:0]
			for _, raw := range lines {
				s.queue = append(s.queue, compute.Line{Raw: raw})
			}
			line := s.queue[0]
			s.queue = s.queue[1:]
			return line, true, nil
		}
	}
}

func (s *brokerStream) Close() error {
	if s.unsub != nil {
		s.unsub()
		s.unsub = nil
	}
	return nil
}
