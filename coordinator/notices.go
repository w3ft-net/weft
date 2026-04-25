package coordinator

import (
	"sync"

	"github.com/kevlarcon/weft/store"
)

// NoticeKind classifies a notice for frontend rendering. The
// frontend decides whether each kind shows as toast / banner /
// modal — backend just emits the hint.
type NoticeKind string

const (
	NoticeKindSuccess NoticeKind = "success"
	NoticeKindWarning NoticeKind = "warning"
	NoticeKindError   NoticeKind = "error"
)

// Notice is the payload shape returned to the API server in the
// query response. The wire shape (protobuf) mirrors this exactly.
type Notice struct {
	Key   string     `json:"key"`
	Kind  NoticeKind `json:"kind"`
	Title string     `json:"title"`
	Body  string     `json:"body"`
}

// noticeRegistry maps notice keys (defined in the package that
// triggers the notice — shellfs.NoticeKeyTmpExpiry, future
// triggers elsewhere) to display content. Adding a notice = one
// entry here plus a call site that signals it. No DB migration.
var noticeRegistry = map[string]Notice{
	"tmp_48h_expiry": {
		Key:   "tmp_48h_expiry",
		Kind:  NoticeKindWarning,
		Title: "/tmp files expire after 48 hours",
		Body:  "Files in /tmp are deleted 48 hours after their last edit. Touching a file (or writing to it) resets the clock. Save anything you want to keep as a watch under /cron.d.",
	},
}

// NoticeQueue collects notices to deliver in a single coordinator
// response. One queue per request; the coordinator builds it at the
// top of runLocal and Flush()s it before returning.
//
// Maybe(key) is the one signaling primitive — anything in the
// request stack (currently shellfs, potentially elsewhere) can call
// it without knowing whether the user has seen this notice before;
// the queue checks the per-user store flag and skips silently if so.
//
// Thread-safe: pipelines fork goroutines for stages, any of which
// might trigger a notice.
type NoticeQueue struct {
	store       *store.Store
	userID      string
	mu          sync.Mutex
	queued      []Notice
	sessionSeen map[string]bool
}

// NewNoticeQueue returns a queue scoped to one request. userID
// empty = no notices delivered (system invocation, automated path);
// Maybe is a no-op in that mode.
func NewNoticeQueue(s *store.Store, userID string) *NoticeQueue {
	return &NoticeQueue{
		store:       s,
		userID:      userID,
		sessionSeen: map[string]bool{},
	}
}

// Maybe queues the notice for delivery iff the user hasn't seen it
// before. Idempotent within and across calls in the same session.
// No-op when userID is empty or the registry has no entry for the
// key (silent skip — registry drift shouldn't break a request).
func (q *NoticeQueue) Maybe(key string) {
	if q == nil || q.userID == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.sessionSeen[key] {
		return
	}
	if q.store != nil {
		seen, err := q.store.HasSeenNotice(q.userID, key)
		if err == nil && seen {
			q.sessionSeen[key] = true
			return
		}
	}
	n, ok := noticeRegistry[key]
	if !ok {
		return
	}
	q.queued = append(q.queued, n)
	q.sessionSeen[key] = true
}

// Flush returns the queued notices and marks them as seen in the
// store, in one shot. Called by the coordinator just before emitting
// the response. Returns nil when nothing is queued.
//
// Marking-on-flush rather than marking-on-dismiss is a tradeoff: if
// the user closes the browser before the notice renders, they miss
// it. The alternative requires a round-trip we don't engineer for
// v1. Documented in docs/plans/notices-and-fs-attribution.md.
func (q *NoticeQueue) Flush() []Notice {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.queued) == 0 {
		return nil
	}
	out := q.queued
	q.queued = nil
	if q.store != nil {
		for _, n := range out {
			_ = q.store.MarkNoticeSeen(q.userID, n.Key, string(n.Kind))
		}
	}
	return out
}
