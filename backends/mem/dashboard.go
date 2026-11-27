package mem

import (
	"sort"
	"sync"
	"time"

	"github.com/w3ft-net/weft/backends/dashboard"
	"github.com/w3ft-net/weft/shape"
)

// Dashboards is a dashboard.Store in memory.
type Dashboards struct {
	mu   sync.Mutex
	rows map[string]dashboard.Dashboard
}

var _ dashboard.Store = (*Dashboards)(nil)

// NewDashboards is an empty store.
func NewDashboards() *Dashboards { return &Dashboards{rows: map[string]dashboard.Dashboard{}} }

func (s *Dashboards) Save(_ shape.Added, user, name, yamlBody string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	d, ok := s.rows[name]
	if !ok {
		s.rows[name] = dashboard.Dashboard{Name: name, YAMLBody: yamlBody, CreatedBy: user, CreatedAt: now, UpdatedAt: now}
		return nil
	}
	if d.YAMLBody == yamlBody {
		return nil
	}
	d.YAMLBody, d.UpdatedAt = yamlBody, now
	s.rows[name] = d
	return nil
}

func (s *Dashboards) Get(_ shape.Added, name string) (*dashboard.Dashboard, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.rows[name]
	if !ok {
		return nil, dashboard.ErrNotFound
	}
	return &d, nil
}

func (s *Dashboards) List(shape.Added) ([]dashboard.Dashboard, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []dashboard.Dashboard
	for _, d := range s.rows {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Dashboards) Delete(_ shape.Added, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, name)
	return nil
}
