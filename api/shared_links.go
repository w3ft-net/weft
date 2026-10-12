package api

import (
	"encoding/json"
	"net/http"
)

// handleGetSharedLink implements GET /api/shared-links/{slug}: what a
// `clipboard` short link points at. It resolves only within the caller's
// active org; anything else is a 404, so a slug leaks nothing across orgs.
func (s *Server) handleGetSharedLink(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	l, err := s.store.GetSharedLink(custID, r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Bin   string `json:"bin"`
		Range string `json:"range"`
		Args  string `json:"args"`
	}{l.Bin, l.RangeSpec, l.Args})
}
