// Package heartbeat tracks which local services have recently said
// they're alive, exposes an HTTP handler that services POST to, and
// produces periodic snapshots a metrics emitter can ship as records.
//
// All exported methods are safe for concurrent use. The handler
// shape is deliberately tiny — POST /h/<service> → 204 — so it can
// be hit by anything that speaks HTTP without an SDK. See
// docs/plans/app-heartbeats.md for the wire-protocol spec.
package heartbeat

import (
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	maxNameLen        = 128
	defaultMaxLimit   = 1024
	defaultStaleAfter = 60 * time.Second
)

// validNamePattern restricts service names to URL-path-safe chars.
// The validation runs on every POST so it has to be cheap.
var validNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// Heartbeats is a bounded in-memory map of service → last-seen-at.
// Hits beyond the configured cap evict the oldest entry, so a
// runaway caller inventing service names per request can't grow
// memory without bound.
type Heartbeats struct {
	mu         sync.Mutex
	last       map[string]time.Time
	max        int
	staleAfter time.Duration
	// now is overridable for tests; production uses time.Now.
	now func() time.Time
}

// Snapshot is one row of the table at the moment Snapshot() ran.
type Snapshot struct {
	Service  string
	LastSeen time.Time
	Age      time.Duration
}

// New constructs a heartbeats table. max is the LRU cap; values
// ≤ 0 fall back to the default. staleAfter is how long a service
// can be silent before Snapshot drops it; values ≤ 0 fall back to
// the default.
func New(max int, staleAfter time.Duration) *Heartbeats {
	if max <= 0 {
		max = defaultMaxLimit
	}
	if staleAfter <= 0 {
		staleAfter = defaultStaleAfter
	}
	return &Heartbeats{
		last:       make(map[string]time.Time),
		max:        max,
		staleAfter: staleAfter,
		now:        time.Now,
	}
}

// Hit records a heartbeat for service. Returns false if the name
// fails validation. Last-write-wins; a service heartbeating 1000
// times a second is fine — each call is one mutex acquisition,
// no I/O.
func (h *Heartbeats) Hit(service string) bool {
	if !validName(service) {
		return false
	}
	now := h.now()

	h.mu.Lock()
	defer h.mu.Unlock()

	if _, present := h.last[service]; !present && len(h.last) >= h.max {
		h.evictOldestLocked()
	}
	h.last[service] = now
	return true
}

// Snapshot returns the current state, dropping (in-place) any
// service whose last heartbeat is older than staleAfter. The
// drop-on-read posture means absence-from-snapshot is itself a
// liveness signal: a service that goes silent for >staleAfter
// stops appearing in subsequent snapshots.
func (h *Heartbeats) Snapshot() []Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := h.now()
	out := make([]Snapshot, 0, len(h.last))
	for service, last := range h.last {
		age := now.Sub(last)
		if age > h.staleAfter {
			delete(h.last, service)
			continue
		}
		out = append(out, Snapshot{
			Service:  service,
			LastSeen: last,
			Age:      age,
		})
	}
	return out
}

// Len returns the current number of tracked services. Useful for
// tests and capacity diagnostics.
func (h *Heartbeats) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.last)
}

// Handler returns an http.Handler that accepts POST /h/<service>.
// Bind the same handler behind both Unix-socket and TCP listeners
// to keep the wire shape identical.
func (h *Heartbeats) Handler() http.Handler {
	return http.HandlerFunc(h.serveHTTP)
}

func (h *Heartbeats) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/h/") {
		http.NotFound(w, r)
		return
	}
	service := strings.TrimPrefix(r.URL.Path, "/h/")
	if !h.Hit(service) {
		http.Error(w, "invalid service name", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// evictOldestLocked drops the entry with the smallest LastSeen.
// O(n) but n ≤ max (default 1024); not worth a list+map LRU at
// this size. Caller holds h.mu.
func (h *Heartbeats) evictOldestLocked() {
	var oldestKey string
	var oldestT time.Time
	first := true
	for k, t := range h.last {
		if first || t.Before(oldestT) {
			oldestKey = k
			oldestT = t
			first = false
		}
	}
	if oldestKey != "" {
		delete(h.last, oldestKey)
	}
}

// validName enforces the URL-path-safe identifier rule. Empty,
// over-length, and disallowed-char inputs all fail.
func validName(s string) bool {
	if len(s) == 0 || len(s) > maxNameLen {
		return false
	}
	return validNamePattern.MatchString(s)
}
