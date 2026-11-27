package mem

import (
	"sort"
	"sync"
	"time"

	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/shape"
)

// Bins is a bin.Store in memory.
type Bins struct {
	mu     sync.Mutex
	nextID uint
	byName map[string]*binRow
}

type binRow struct {
	bin.Script
	deleted bool
}

var _ bin.Store = (*Bins)(nil)

// NewBins is an empty store.
func NewBins() *Bins { return &Bins{byName: map[string]*binRow{}} }

func (s *Bins) Save(_ shape.Added, user, name, source, language string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := bin.ContentHash(source)
	now := time.Now().UTC()
	r, ok := s.byName[name]
	if !ok {
		s.nextID++
		s.byName[name] = &binRow{Script: bin.Script{ID: s.nextID, Name: name, Source: source, Language: language,
			ContentHash: hash, CreatedBy: user, AuthoredBy: user, CreatedAt: now, UpdatedAt: now}}
		return nil
	}
	if r.ContentHash == hash && !r.deleted {
		return nil
	}
	r.Source, r.Language, r.ContentHash, r.Message, r.AuthoredBy, r.UpdatedAt, r.deleted = source, language, hash, "", user, now, false
	return nil
}

func (s *Bins) Get(_ shape.Added, name string) (*bin.Script, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byName[name]
	if !ok || r.deleted {
		return nil, bin.ErrNotFound
	}
	cp := r.Script
	return &cp, nil
}

func (s *Bins) List(shape.Added) ([]bin.Script, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []bin.Script
	for _, r := range s.byName {
		if !r.deleted {
			out = append(out, r.Script)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Bins) Delete(_ shape.Added, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.byName[name]; ok {
		r.deleted = true
	}
	return nil
}

func (s *Bins) AmendMessage(_ shape.Added, authorID, name, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byName[name]
	if !ok || r.deleted {
		return bin.ErrNotFound
	}
	if authorID != "" && r.AuthoredBy != authorID {
		return bin.ErrNotYours
	}
	r.Message = message
	return nil
}
