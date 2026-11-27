package mem

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/w3ft-net/weft/backends/dlq"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

var _ dlq.Store = (*DLQ)(nil)

// DLQ is the open build's dead-letter store: batches held in memory, in
// the encoding every dlq store uses, until a replay delivers them. A restart
// loses them; a build that cannot accept that supplies a durable dlq.Store.
type DLQ struct {
	mu      sync.Mutex
	next    int64
	batches []dlqBatch
}

type dlqBatch struct {
	id      int64
	payload []byte
}

// NewDLQ is an empty in-memory dead-letter store.
func NewDLQ() *DLQ { return &DLQ{} }

func (s *DLQ) Write(_ context.Context, added shape.Added, rows []*pb.LogRow) (string, error) {
	if len(rows) == 0 {
		return "", nil
	}
	payload, err := dlq.Encode(added, rows)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	s.batches = append(s.batches, dlqBatch{id: s.next, payload: payload})
	return "memory:dead_letter_batches/" + strconv.FormatInt(s.next, 10), nil
}

func (s *DLQ) Replay(_ context.Context, sink func([]dlq.Entry)) (int, error) {
	// The batches are read before any is delivered: a sink may write a new one.
	s.mu.Lock()
	pending := append([]dlqBatch(nil), s.batches...)
	s.mu.Unlock()

	var (
		total int
		errs  []error
	)
	for _, b := range pending {
		entries, err := dlq.Decode(bytes.NewReader(b.payload))
		if err != nil {
			errs = append(errs, fmt.Errorf("decode batch %d: %w", b.id, err))
			continue
		}
		sink(entries)
		total += len(entries)
		s.remove(b.id)
	}
	return total, errors.Join(errs...)
}

func (s *DLQ) remove(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, b := range s.batches {
		if b.id == id {
			s.batches = append(s.batches[:i], s.batches[i+1:]...)
			return
		}
	}
}
