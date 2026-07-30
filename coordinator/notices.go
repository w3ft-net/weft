package coordinator

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/graybeardcorp/weft/store"
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

// NoticeKeyMemberRetainedGrants is emitted by migration 072's membership
// trigger when an admin is demoted to member: the four org_permissions
// bits their promotion materialized stay put, and the operator who
// demoted them is told exactly what was kept.
const NoticeKeyMemberRetainedGrants = "member_retained_grants"

// NoticeKeyOnboardingGraduated is emitted when a user finishes the Loom
// tour. The browser renders it as the graduation banner; the shell
// renders it as a notice like any other. Whichever surface the user
// reaches first delivers it — it is one notice with two renderers, not
// a browser feature that happens to live in this table.
const NoticeKeyOnboardingGraduated = "onboarding_graduated"

// noticeTemplate is a registry entry: the presentation half of a notice.
// The database row carries only params — the facts — because the browser
// and the TUI render differently and a trigger has no business writing
// either one's copy. render turns one into the other.
type noticeTemplate struct {
	kind   NoticeKind
	render func(params map[string]any) (title, body string)
}

// noticeRegistry maps notice keys (defined in the package that
// triggers the notice — mounts.NoticeKeyTmpExpiry, the trigger keys
// here, future ones elsewhere) to display content. Adding a notice =
// one entry here plus a call site that signals it. No DB migration.
var noticeRegistry = map[string]noticeTemplate{
	"tmp_48h_expiry": {
		kind: NoticeKindWarning,
		render: func(map[string]any) (string, string) {
			return "/tmp files expire after 48 hours",
				"Files in /tmp are deleted 48 hours after their last edit. Touching a file (or writing to it) resets the clock. Save anything you want to keep as a cron entry under /cron.d."
		},
	},
	NoticeKeyOnboardingGraduated: {
		kind: NoticeKindSuccess,
		render: func(map[string]any) (string, string) {
			return "You've finished the tour",
				"You've seen the whole shell against Loom Inc's data. Your own org is ready when you are — connect a log source and the same pipelines work on your logs."
		},
	},
	NoticeKeyMemberRetainedGrants: {
		kind: NoticeKindWarning,
		render: func(p map[string]any) (string, string) {
			org := paramString(p, "org")
			member := paramString(p, "member")
			groups := paramStrings(p, "groups")
			// Lead with the org. This notice is addressed to a user, not
			// to one of their memberships, so it can surface with any org
			// selected in the switcher — including one it has nothing to
			// do with.
			title := fmt.Sprintf("%s kept their groups in %s", member, org)
			if len(groups) == 0 {
				// Belt and braces: the trigger only fires the notice when
				// there is something to report, but a hand-written row
				// shouldn't render as an empty sentence.
				return title, fmt.Sprintf("In %s, %s is no longer an admin. They hold no group grants.", org, member)
			}
			return title, fmt.Sprintf(
				"In %s, %s is no longer an admin, but the grants their promotion created are still in place: %s. "+
					"Demotion never revokes — take them back explicitly with `deluser %s <group>`.",
				org, member, strings.Join(groups, ", "), member)
		},
	},
}

// paramString reads a string param, tolerating absence — a missing fact
// renders as an empty slot, not a panic in a response path.
func paramString(p map[string]any, key string) string {
	s, _ := p[key].(string)
	return s
}

// paramStrings reads a string-list param. JSON round-tripping turns a
// list into []any, so the elements need individual asserts.
func paramStrings(p map[string]any, key string) []string {
	raw, _ := p[key].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// RenderNotice turns a queued row into its display form. Reports false
// when the key has no registry entry — registry/DB drift shouldn't put
// a blank card in front of a user, and the row stays pending so the
// notice survives a deploy that adds the entry.
func RenderNotice(p store.PendingNotice) (Notice, bool) {
	tmpl, ok := noticeRegistry[p.Key]
	if !ok {
		return Notice{}, false
	}
	title, body := tmpl.render(p.Params)
	return Notice{Key: p.Key, Kind: tmpl.kind, Title: title, Body: body}, true
}

// NoticeQueue is the request-scoped half of notice delivery: Maybe()
// enqueues, Drain() delivers. One queue per request; the coordinator
// builds it at the top of runLocal and Drain()s it before returning.
//
// Enqueuing writes a row rather than buffering in memory, so a notice
// signaled by a stage that later fails still reaches the user, and so
// notices left by out-of-band emitters (the membership trigger) ride
// out on the same path.
//
// Thread-safe: pipelines fork goroutines for stages, any of which
// might signal a notice.
type NoticeQueue struct {
	store  *store.Store
	userID string
	mu     sync.Mutex
	// signaled dedups within one request. A pipeline forking a
	// goroutine per stage must not write 500 identical rows for one
	// command. Across requests there is no dedup at all — that is
	// emitter policy now, deliberately.
	signaled map[string]bool
}

// NewNoticeQueue returns a queue scoped to one request. userID
// empty = no notices delivered (system invocation, automated path);
// Maybe and Drain are no-ops in that mode.
func NewNoticeQueue(s *store.Store, userID string) *NoticeQueue {
	return &NoticeQueue{
		store:    s,
		userID:   userID,
		signaled: map[string]bool{},
	}
}

// Maybe queues the notice for delivery. "Maybe" is now only about the
// per-request dedup and the registry check — it no longer asks whether
// the user has ever seen this key, so an emitter that fires on every
// call now produces a notice on every request. That is the intended
// trade: over-firing is a visible bug in the emitter rather than
// something the storage layer silently absorbs.
func (q *NoticeQueue) Maybe(key string) {
	if q == nil || q.userID == "" || q.store == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.signaled[key] {
		return
	}
	tmpl, ok := noticeRegistry[key]
	if !ok {
		return
	}
	q.signaled[key] = true
	if err := q.store.EmitNotice(q.userID, key, string(tmpl.kind), nil); err != nil {
		log.Printf("notices: emit %s for %s: %v", key, q.userID, err)
	}
}

// Drain returns the user's pending notices and marks them delivered.
// It picks up both what this request signaled and anything queued out
// of band since the user was last seen. Returns nil when the queue is
// empty.
//
// Marking on delivery rather than on dismissal is still a tradeoff, but
// a bounded one now: the row is per-instance, so a connection that drops
// before the notice renders costs one missed render, not the only copy
// the user was ever going to get.
func (q *NoticeQueue) Drain() []Notice {
	if q == nil || q.userID == "" || q.store == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	pending, err := q.store.PendingNotices(q.userID)
	if err != nil {
		log.Printf("notices: load pending for %s: %v", q.userID, err)
		return nil
	}

	out := make([]Notice, 0, len(pending))
	ids := make([]int64, 0, len(pending))
	for _, p := range pending {
		n, ok := RenderNotice(p)
		if !ok {
			continue // unknown key: leave it pending, don't burn it
		}
		out = append(out, n)
		ids = append(ids, p.ID)
	}
	if err := q.store.MarkNoticesSeen(ids); err != nil {
		log.Printf("notices: mark seen for %s: %v", q.userID, err)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
