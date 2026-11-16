// Package proposals stages MCP-server-authored mutations to disk
// before they take effect. Every weft.{script,cron}_propose tool
// call lands as a JSON file under the session directory; the
// human-side `weft propose review` reads them back, displays diffs,
// and applies the ones the human accepts.
//
// The on-disk format is versioned implicitly through the JSON tag
// names — we read with permissive defaults so older proposals
// remain decodable when fields are added.
package proposals

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Proposal is one staged mutation. Kind is the high-level
// classifier (script, cron, delete) and SubKind disambiguates
// what a delete proposal targets.
type Proposal struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`              // "script" | "cron" | "delete"
	SubKind   string    `json:"subKind,omitempty"` // for "delete": "script" | "cron"
	Name      string    `json:"name"`
	Rationale string    `json:"rationale,omitempty"`
	Current   string    `json:"current"`  // current backend state ("" if creating)
	Proposed  string    `json:"proposed"` // new state ("" for delete)
	Diff      string    `json:"diff"`     // unified diff Current → Proposed
	CreatedAt time.Time `json:"createdAt"`
}

// Store is a directory of proposal files. Each MCP server session
// gets its own subdirectory so concurrent sessions don't collide.
type Store struct {
	dir string
}

// ErrNotFound is returned by Get/Remove when the ID is unknown.
var ErrNotFound = errors.New("proposal not found")

// New opens (and creates if needed) a store rooted at dir. dir is
// typically ~/.weft/proposals/<session-id>/ in production and
// t.TempDir() in tests.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Dir returns the directory the store is anchored at. Useful for
// printing review-time hints to the human ("12 proposals in /...").
func (s *Store) Dir() string { return s.dir }

// Add stages a new proposal. The Diff field is computed from
// Current and Proposed using the resource's natural path label
// (e.g. bin/<name>) so the human reading it sees something that
// looks like a real source-control diff. The supplied p.ID,
// p.Diff, p.CreatedAt are ignored — Add owns those fields.
func (s *Store) Add(p Proposal) (string, error) {
	id, err := s.nextID()
	if err != nil {
		return "", err
	}
	p.ID = id
	p.CreatedAt = time.Now().UTC()
	p.Diff = renderDiff(p)

	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	path := filepath.Join(s.dir, p.ID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return id, nil
}

// Get reads one proposal back. The returned struct is a copy; the
// caller is free to mutate it.
func (s *Store) Get(id string) (Proposal, error) {
	var p Proposal
	path := filepath.Join(s.dir, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return p, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return p, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("unmarshal %s: %w", path, err)
	}
	return p, nil
}

// List returns every staged proposal in ID order. The IDs are
// zero-padded so lexicographic sort matches creation order through
// 9999 proposals — far more than any session should accumulate.
func (s *Store) List() ([]Proposal, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", s.dir, err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(name, ".json"))
	}
	sort.Strings(ids)
	out := make([]Proposal, 0, len(ids))
	for _, id := range ids {
		p, err := s.Get(id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Remove deletes one proposal. Used by Apply once the underlying
// mutation has succeeded — and by `weft propose clear` when the
// human discards the staged batch.
func (s *Store) Remove(id string) error {
	path := filepath.Join(s.dir, id+".json")
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// nextID picks the smallest ID not yet taken. We don't trust an
// in-memory counter because the store may be opened multiple times
// during the life of a single MCP session (e.g. a crash + restart).
// A directory scan is O(n) but n is small (single-digit dozens at
// most) and only happens on Add.
func (s *Store) nextID() (string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return "", err
	}
	max := 0
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".json")
		var n int
		if _, err := fmt.Sscanf(name, "%d", &n); err == nil && n > max {
			max = n
		}
	}
	return fmt.Sprintf("%04d", max+1), nil
}

// isNotFound is exported via ErrNotFound but tests want a smaller
// dependency surface. Keeping a helper local avoids forcing every
// test file to import errors.
func isNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}
