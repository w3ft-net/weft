package api

import (
	"encoding/json"
	"net/http"

	"github.com/kevlarcon/weft/ingest"
	"github.com/kevlarcon/weft/store"
)

// ── Agent-facing endpoints (token-authenticated) ───────────

// handleAgentSample receives sample log lines from weftd for a new source.
// POST /api/agent/sample
func (s *Server) handleAgentSample(w http.ResponseWriter, r *http.Request) {
	agent := GetAgentAuth(r)
	if agent == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		App       string   `json:"app"`
		Instance  string   `json:"instance"`
		HostGroup string   `json:"host_group"`
		Lines     []string `json:"lines"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.App == "" || len(req.Lines) == 0 {
		http.Error(w, "app and lines required", http.StatusBadRequest)
		return
	}

	// Run schema detection on the sample.
	detection := ingest.Detect(req.Lines)

	sampleJSON, _ := json.Marshal(req.Lines)
	detectionJSON, _ := json.Marshal(detection)

	if err := s.store.UpsertPendingSource(
		agent.CustomerID, agent.Namespace, req.App,
		req.Instance, req.HostGroup,
		string(sampleJSON), string(detectionJSON),
	); err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
}

// handleAgentSourceStatus returns the configuration status of a source.
// GET /api/agent/source-status?app=NAME
func (s *Server) handleAgentSourceStatus(w http.ResponseWriter, r *http.Request) {
	agent := GetAgentAuth(r)
	if agent == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	app := r.URL.Query().Get("app")
	if app == "" {
		http.Error(w, "app parameter required", http.StatusBadRequest)
		return
	}

	// Check if fully configured (has a schema in log_groups).
	configured, schemaYAML, err := s.store.SourceIsConfigured(agent.CustomerName, agent.Namespace, app)
	if err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if configured {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "configured",
			"schema": schemaYAML,
		})
		return
	}

	// Check if pending (sample submitted, awaiting review).
	ps, err := s.store.GetPendingSource(agent.CustomerID, agent.Namespace, app)
	if err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if ps != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
		return
	}

	// Unknown source.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "unknown"})
}

// ── Dashboard/CLI endpoints (JWT-authenticated) ────────────

// handlePendingSources lists pending sources for the authenticated customer.
// GET /api/sources/pending[?namespace=X]
func (s *Server) handlePendingSources(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	customer, err := s.store.GetCustomerByName(claims.CustomerName)
	if err != nil {
		http.Error(w, "customer not found", http.StatusNotFound)
		return
	}

	namespace := r.URL.Query().Get("namespace")
	sources, err := s.store.ListPendingSources(customer.ID, namespace)
	if err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	type pendingResp struct {
		App       string          `json:"app"`
		Namespace string          `json:"namespace"`
		Instance  string          `json:"instance"`
		HostGroup string          `json:"host_group"`
		Sample    json.RawMessage `json:"sample"`
		Detection json.RawMessage `json:"detection"`
		CreatedAt string          `json:"created_at"`
	}

	var resp []pendingResp
	for _, ps := range sources {
		resp = append(resp, pendingResp{
			App:       ps.App,
			Namespace: ps.Namespace,
			Instance:  ps.Instance,
			HostGroup: ps.HostGroup,
			Sample:    json.RawMessage(ps.Sample),
			Detection: json.RawMessage(ps.Detection),
			CreatedAt: ps.CreatedAt.Format("2006-01-02T15:04:05Z"),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleConfigureSource confirms the schema for a pending source.
// POST /api/sources/{namespace}/{app}/configure
func (s *Server) handleConfigureSource(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	namespace := r.PathValue("namespace")
	app := r.PathValue("app")
	if namespace == "" || app == "" {
		http.Error(w, "namespace and app required", http.StatusBadRequest)
		return
	}

	customer, err := s.store.GetCustomerByName(claims.CustomerName)
	if err != nil {
		http.Error(w, "customer not found", http.StatusNotFound)
		return
	}

	// Verify the pending source exists for this customer.
	ps, err := s.store.GetPendingSource(customer.ID, namespace, app)
	if err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if ps == nil {
		http.Error(w, "no pending source found", http.StatusNotFound)
		return
	}

	var req struct {
		Format          string             `json:"format"`
		TimestampField  string             `json:"timestamp_field"`
		TimestampFormat string             `json:"timestamp_format"`
		LevelField      string             `json:"level_field"`
		LevelPattern    string             `json:"level_pattern"`
		ExpandKeys      []ingest.ExpandKey `json:"expand_keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Build schema.
	fullApp := namespace + "/" + app
	schema := &ingest.Schema{
		App:             fullApp,
		Customer:        claims.CustomerName,
		Format:          req.Format,
		TimestampField:  req.TimestampField,
		TimestampFormat: req.TimestampFormat,
		LevelField:      req.LevelField,
		LevelPattern:    req.LevelPattern,
		ExpandKeys:      req.ExpandKeys,
	}

	schemaYAML, err := schema.ToYAML()
	if err != nil {
		http.Error(w, "schema marshal error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Store the configured source. For weftd-sourced logs we don't have
	// an AWS account/log group — we write a placeholder "agent" account
	// and mark the LogGroup with source_type=weftd so the poller knows
	// not to treat it as a CloudWatch target.
	acct, err := s.store.FindOrCreateAccount(customer.ID, "agent", "", "")
	if err != nil {
		http.Error(w, "account error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.store.AddLogGroup(
		acct.ID, store.SourceTypeWeftd, fullApp, fullApp,
		"local", string(schemaYAML), "0s",
	); err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Remove from pending.
	if err := s.store.DeletePendingSource(customer.ID, namespace, app); err != nil {
		http.Error(w, "cleanup error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "configured"})
}
