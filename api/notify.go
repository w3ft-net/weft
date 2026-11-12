package api

import (
	"encoding/json"
	"net/http"

	"github.com/graybeardcorp/warp/pkg/shellcore"
)

// requireConfigWrite refuses (403) unless the caller may change /etc, the
// group (configmgr) that owns notify.yaml. Integrations are the credentials
// its aliases deliver through, so they take the same group, and the check is
// the /etc mount's own rather than a second copy of it.
func (s *Server) requireConfigWrite(w http.ResponseWriter, r *http.Request, session Session) bool {
	if err := s.shellFS(r, session, nil).MountFS().Access("/etc/notify.yaml", shellcore.PermWrite); err != nil {
		writeFSError(w, err)
		return false
	}
	return true
}

// handleGetNotifyConfig returns the customer's /etc/notify.yaml YAML.
// An absent row yields an empty string — the shell treats that as a blank file.
func (s *Server) handleGetNotifyConfig(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	nc, _ := s.store.GetNotifyConfig(custID)
	yaml := ""
	if nc != nil {
		yaml = nc.AliasesYAML
	}
	json.NewEncoder(w).Encode(map[string]string{"aliases_yaml": yaml})
}

// handleSaveNotifyConfig validates and persists the customer's notify.yaml.
// Validation mirrors the in-process shellfs path so both shells get the same
// errors back (missing targets, bad PD severity, malformed YAML).
func (s *Server) handleSaveNotifyConfig(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if _, ok := activeOrg(w, session); !ok {
		return
	}
	var req struct {
		AliasesYAML string `json:"aliases_yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	// The /etc mount validates the aliases.
	if err := s.shellFS(r, session, nil).WriteFile("/etc/notify.yaml", []byte(req.AliasesYAML)); err != nil {
		writeFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleWebhookSecretSave creates or replaces a named webhook secret, the
// credential a webhook alert target delivers with.
// Body: {"name": "token", "value": "<secret>"}.
func (s *Server) handleWebhookSecretSave(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if !s.requireConfigWrite(w, r, session) {
		return
	}
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	var req struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.Value == "" {
		http.Error(w, "name and value required", http.StatusBadRequest)
		return
	}
	if err := s.store.SaveWebhookSecret(custID, req.Name, req.Value); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleWebhookSecretDelete removes a webhook secret that no alias uses.
// Path: DELETE /api/integrations/webhook-secrets/{name}
func (s *Server) handleWebhookSecretDelete(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if !s.requireConfigWrite(w, r, session) {
		return
	}
	custID, ok := activeOrg(w, session)
	if !ok {
		return
	}
	if err := s.store.DeleteWebhookSecret(custID, r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
