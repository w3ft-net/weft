package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/graybeardcorp/weft/notices"
)

// Notices reach the TUI for free: they ride out on the query response
// that the shell was already making (notices.Queue.Drain).
// The browser has no guaranteed shell request, so it polls here.
//
// The two surfaces differ in when they mark: the shell marks on the way
// out, because the response it is attached to is the delivery. The
// browser marks on dismissal, because a fetched-but-unrendered notice is
// a real possibility for a page that may navigate away mid-flight.

// noticeResponse is one pending notice as the SPA sees it: the rendered
// presentation plus the id it needs to dismiss the row.
type noticeResponse struct {
	ID    int64  `json:"id"`
	Key   string `json:"key"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// handleListNotices returns the caller's undelivered notices, oldest
// first. Reading does not mark them seen — POST /api/notices/{id}/seen
// does that, once the SPA has actually shown one.
func (s *Server) handleListNotices(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)

	pending, err := s.store.PendingNotices(session.UserID)
	if err != nil {
		log.Printf("notices: list for %s: %v", session.UserID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	out := make([]noticeResponse, 0, len(pending))
	for _, p := range pending {
		n, ok := notices.Render(p)
		if !ok {
			// A key this build doesn't know. Leave it pending rather
			// than shipping a blank card; a deploy that adds the
			// registry entry will deliver it.
			continue
		}
		out = append(out, noticeResponse{
			ID:    p.ID,
			Key:   n.Key,
			Kind:  string(n.Kind),
			Title: n.Title,
			Body:  n.Body,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"notices": out})
}

// handleNoticeSeen marks one notice instance delivered. Scoped to the
// caller's own rows: an id belonging to someone else marks nothing and
// is reported as 404, so the endpoint can't be used to probe for or
// burn another user's notices.
func (s *Server) handleNoticeSeen(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid notice id", http.StatusBadRequest)
		return
	}

	pending, err := s.store.PendingNotices(session.UserID)
	if err != nil {
		log.Printf("notices: seen lookup for %s: %v", session.UserID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	found := false
	for _, p := range pending {
		if p.ID == id {
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "no such pending notice", http.StatusNotFound)
		return
	}

	if err := s.store.MarkNoticesSeen([]int64{id}); err != nil {
		log.Printf("notices: mark %d seen for %s: %v", id, session.UserID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
