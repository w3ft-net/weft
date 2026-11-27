package mem

import (
	"sort"
	"sync"
	"time"

	"github.com/w3ft-net/weft/backends/ingestfail"
	"github.com/w3ft-net/weft/shape"
)

// IngestFailures is an ingestfail.Store in memory.
type IngestFailures struct {
	mu   sync.Mutex
	rows []ingestfail.Failure
}

var _ ingestfail.Store = (*IngestFailures)(nil)

// NewIngestFailures is an empty store.
func NewIngestFailures() *IngestFailures { return &IngestFailures{} }

func (s *IngestFailures) Record(f ingestfail.Failure) error {
	if f.At.IsZero() {
		f.At = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, f)
	return nil
}

func (s *IngestFailures) AffectedServices(_ shape.Added, since time.Time) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, f := range s.rows {
		if f.At.Before(since) {
			continue
		}
		for _, svc := range f.Services {
			if !seen[svc] {
				seen[svc] = true
				out = append(out, svc)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
