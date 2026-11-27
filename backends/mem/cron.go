package mem

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/shape"
)

// Crons is a cron.Store in memory over the namespaces of a Sources: a watch is
// homed in one (or at the owner level, namespace 0), and deleting the namespace
// deletes its watches.
type Crons struct {
	mu        sync.Mutex
	nextWatch uint
	nextRes   uint
	nextInc   int
	watches   []*cronRow
	results   []cron.Result
	incidents []*cron.Incident
	delivered map[string]map[string]bool
}

type cronRow struct {
	cron.Watch
	deleted bool
}

var _ cron.Store = (*Crons)(nil)

// NewCrons is an empty store over src's namespaces.
func NewCrons(src *Sources) *Crons {
	c := &Crons{delivered: map[string]map[string]bool{}}
	src.mu.Lock()
	src.onDeleteNamespace = append(src.onDeleteNamespace, c.dropNamespace)
	src.mu.Unlock()
	return c
}

func (s *Crons) dropNamespace(id uint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gone := map[uint]bool{}
	kept := s.watches[:0]
	for _, w := range s.watches {
		if w.Namespace == id {
			gone[w.ID] = true
		} else {
			kept = append(kept, w)
		}
	}
	s.watches = kept
	rs := s.results[:0]
	for _, r := range s.results {
		if !gone[r.WatchID] {
			rs = append(rs, r)
		}
	}
	s.results = rs
	incs := s.incidents[:0]
	for _, inc := range s.incidents {
		if gone[inc.WatchID] {
			delete(s.delivered, inc.ID)
		} else {
			incs = append(incs, inc)
		}
	}
	s.incidents = incs
}

func (s *Crons) live(ns uint, name string) *cronRow {
	for _, w := range s.watches {
		if !w.deleted && w.Namespace == ns && w.Name == name {
			return w
		}
	}
	return nil
}

func (s *Crons) byID(id uint) *cronRow {
	for _, w := range s.watches {
		if w.ID == id {
			return w
		}
	}
	return nil
}

func cpWatch(w *cronRow) *cron.Watch {
	c := w.Watch
	c.WatchSpec.Schedule, c.WatchSpec.Pipeline, c.WatchSpec.Enabled = c.Schedule, c.Pipeline, c.Enabled
	c.WatchSpec.LoweredPlan = c.LoweredPlan
	return &c
}

func (s *Crons) SaveWatch(_ shape.Added, ns uint, author, name string, w cron.WatchSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := cron.ContentHash(w.Schedule, w.Pipeline)
	now := time.Now().UTC()
	c := w.Compiled
	if c.ValueType == "" {
		c.ValueType = cron.ValueNumber
	}
	// A deleted watch of the name is revived; a live one overwritten.
	var row *cronRow
	for _, x := range s.watches {
		if x.Namespace == ns && x.Name == name {
			row = x
		}
	}
	if row == nil {
		s.nextWatch++
		row = &cronRow{Watch: cron.Watch{ID: s.nextWatch, Namespace: ns, Name: name, CreatedBy: author, CreatedAt: now}}
		s.watches = append(s.watches, row)
	} else if row.ContentHash == hash && !row.deleted && row.Enabled == w.Enabled {
		return nil
	}
	wasEnabled := row.Enabled
	fresh := row.ContentHash == ""
	row.deleted = false
	row.Schedule, row.Pipeline, row.Enabled, row.Compiled, row.LoweredPlan = w.Schedule, w.Pipeline, w.Enabled, c, w.LoweredPlan
	row.ContentHash, row.Message, row.AuthoredBy, row.UpdatedAt = hash, "", author, now
	if !fresh && w.Enabled && !wasEnabled {
		row.ConsecutiveStrikes = 0
	}
	return nil
}

func (s *Crons) GetWatch(_ shape.Added, ns uint, name string) (*cron.Watch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.live(ns, name); w != nil {
		return cpWatch(w), nil
	}
	return nil, cron.ErrNotFound
}

func (s *Crons) ListWatches(shape.Added) ([]cron.Watch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []cron.Watch
	for _, w := range s.watches {
		if !w.deleted {
			out = append(out, *cpWatch(w))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *Crons) AmendMessage(_ shape.Added, ns uint, authorID, name, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.live(ns, name)
	if w == nil {
		return cron.ErrNotFound
	}
	if authorID != "" && w.AuthoredBy != authorID {
		return cron.ErrNotYours
	}
	w.Message = message
	return nil
}

func (s *Crons) DeleteWatch(_ shape.Added, ns uint, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.live(ns, name); w != nil {
		w.deleted = true
	}
	return nil
}

func (s *Crons) MoveWatch(_ shape.Added, fromNS uint, fromName string, toNS uint, toName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.live(fromNS, fromName)
	if w == nil {
		return cron.ErrNotFound
	}
	if s.live(toNS, toName) != nil {
		return cron.ErrExists
	}
	// A deleted watch that held the name is gone for good: the name is the new home's.
	kept := s.watches[:0]
	for _, x := range s.watches {
		if !(x.deleted && x.Namespace == toNS && x.Name == toName) {
			kept = append(kept, x)
		}
	}
	s.watches = kept
	w.Namespace, w.Name, w.UpdatedAt = toNS, toName, time.Now().UTC()
	return nil
}

func (s *Crons) CountEnabled(shape.Added) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, w := range s.watches {
		if !w.deleted && w.Enabled {
			n++
		}
	}
	return n, nil
}

func (s *Crons) ListEnabled(context.Context) ([]cron.Watch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []cron.Watch
	for _, w := range s.watches {
		if !w.deleted && w.Enabled {
			out = append(out, *cpWatch(w))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Crons) RecordRun(_ context.Context, id uint, at time.Time, status string, resetStrikes bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.byID(id)
	if w == nil {
		return cron.ErrNotFound
	}
	at = at.UTC()
	w.LastRunAt, w.LastStatus = &at, status
	if resetStrikes {
		w.ConsecutiveStrikes = 0
	}
	return nil
}

func (s *Crons) SetLoweredPlan(_ context.Context, id uint, plan []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.byID(id)
	if w == nil {
		return cron.ErrNotFound
	}
	if len(plan) == 0 {
		plan = nil
	}
	w.LoweredPlan = plan
	return nil
}

func (s *Crons) RecordStrike(_ context.Context, id uint, limit int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.byID(id)
	if w == nil {
		return false, cron.ErrNotFound
	}
	w.ConsecutiveStrikes++
	w.Enabled = w.Enabled && w.ConsecutiveStrikes < limit
	return !w.Enabled, nil
}

func (s *Crons) Disable(_ context.Context, id uint) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.byID(id)
	if w == nil || !w.Enabled {
		return false, nil
	}
	w.Enabled = false
	return true, nil
}

func (s *Crons) SaveResults(rs []cron.Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rs {
		s.nextRes++
		r.ID = s.nextRes
		r.RanAt = r.RanAt.UTC()
		s.results = append(s.results, r)
	}
	return nil
}

func (s *Crons) forWatch(id uint) []cron.Result {
	var out []cron.Result
	for _, r := range s.results {
		if r.WatchID == id {
			out = append(out, r)
		}
	}
	return out
}

func (s *Crons) ListResults(watchID uint, limit int, tag string) ([]cron.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []cron.Result
	for _, r := range s.forWatch(watchID) {
		if tag == "" || r.Tag == tag {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].RanAt.Equal(out[j].RanAt) {
			return out[i].RanAt.After(out[j].RanAt)
		}
		return out[i].ID > out[j].ID
	})
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Crons) LatestRun(watchID uint) ([]cron.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []cron.Result
	var newest time.Time
	for _, r := range s.forWatch(watchID) {
		if r.Value == nil && r.TextValue == nil {
			continue
		}
		kept = append(kept, r)
		if r.RanAt.After(newest) {
			newest = r.RanAt
		}
	}
	var out []cron.Result
	for _, r := range kept {
		if r.RanAt.Equal(newest) {
			out = append(out, r)
		}
	}
	// Numbers highest first (a row with no number last), then by tag.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Value, out[j].Value
		switch {
		case a != nil && b != nil && *a != *b:
			return *a > *b
		case (a == nil) != (b == nil):
			return a != nil
		}
		return out[i].Tag < out[j].Tag
	})
	return out, nil
}

func (s *Crons) CountResults(watchID uint, since time.Time, tag string, nonEmpty bool) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, r := range s.forWatch(watchID) {
		if r.RanAt.Before(since) || (tag != "" && r.Tag != tag) || (nonEmpty && r.Stdout == "") {
			continue
		}
		n++
	}
	return n, nil
}

// numeric is the watch's numeric results since the time, oldest first.
func (s *Crons) numeric(watchID uint, since time.Time) []cron.Result {
	var out []cron.Result
	for _, r := range s.forWatch(watchID) {
		if r.Value != nil && !r.RanAt.Before(since) {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].RanAt.Equal(out[j].RanAt) {
			return out[i].RanAt.Before(out[j].RanAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *Crons) NumericEnds(watchID uint, since time.Time) (cron.Ends, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.numeric(watchID, since)
	if len(rs) == 0 {
		return cron.Ends{}, nil
	}
	return cron.Ends{First: *rs[0].Value, Last: *rs[len(rs)-1].Value, N: int64(len(rs))}, nil
}

func (s *Crons) NumericPoints(watchID uint, since time.Time) ([]cron.Point, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []cron.Point
	for _, r := range s.numeric(watchID, since) {
		out = append(out, cron.Point{At: r.RanAt, Value: *r.Value})
	}
	return out, nil
}

func (s *Crons) WindowStats(watchID uint, since time.Time, tag string) (cron.WindowStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st cron.WindowStats
	for _, r := range s.numeric(watchID, since) {
		if tag != "" && r.Tag != tag {
			continue
		}
		v := *r.Value
		if st.Count == 0 || v < st.Min {
			st.Min = v
		}
		if st.Count == 0 || v > st.Max {
			st.Max = v
		}
		st.Count++
		st.Sum += v
		st.SumSq += v * v
	}
	return st, nil
}

func (s *Crons) Series(watchID uint, start, end time.Time) (cron.Series, error) {
	out := cron.Series{Granularity: "raw", Start: start, End: end}
	if !start.Before(end) {
		return out, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.numeric(watchID, start)
	sort.SliceStable(rs, func(i, j int) bool {
		if !rs[i].RanAt.Equal(rs[j].RanAt) {
			return rs[i].RanAt.Before(rs[j].RanAt)
		}
		if rs[i].Tag != rs[j].Tag {
			return rs[i].Tag < rs[j].Tag
		}
		return rs[i].ID < rs[j].ID
	})
	for _, r := range rs {
		if !r.RanAt.Before(end) {
			continue
		}
		v := *r.Value
		out.Buckets = append(out.Buckets, cron.Bucket{Start: r.RanAt, Tag: r.Tag, Count: 1, Sum: v, SumSq: v * v, Min: v, Max: v})
	}
	return out, nil
}

func (s *Crons) OpenOrTouchIncident(watchID uint, tag string) (*cron.Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for _, inc := range s.incidents {
		if inc.WatchID == watchID && inc.Tag == tag && inc.ClosedAt == nil {
			inc.LastSeenAt = now
			c := *inc
			return &c, nil
		}
	}
	s.nextInc++
	inc := &cron.Incident{ID: fmt.Sprintf("incident-%d", s.nextInc), WatchID: watchID, Tag: tag, OpenedAt: now, LastSeenAt: now}
	s.incidents = append(s.incidents, inc)
	c := *inc
	return &c, nil
}

func (s *Crons) CloseIncident(watchID uint, tag string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for _, inc := range s.incidents {
		if inc.WatchID == watchID && inc.Tag == tag && inc.ClosedAt == nil {
			inc.ClosedAt = &now
		}
	}
	return nil
}

func (s *Crons) IncidentDeliveries(incidentID string) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	done := map[string]bool{}
	for t := range s.delivered[incidentID] {
		done[t] = true
	}
	return done, nil
}

func (s *Crons) RecordDeliveries(incidentID string, targets []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delivered[incidentID] == nil {
		s.delivered[incidentID] = map[string]bool{}
	}
	for _, t := range targets {
		s.delivered[incidentID][t] = true
	}
	return nil
}

// SetCreatedAt dates a watch's creation, which the store stamps at save: a test
// fixture's way to make a cron older than the slots it simulates.
func (s *Crons) SetCreatedAt(id uint, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w := s.byID(id); w != nil {
		w.CreatedAt = at.UTC()
	}
}
