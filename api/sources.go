package api

import (
	"encoding/json"
	"net/http"

	"github.com/kevlarcon/weft/auth"
	"github.com/kevlarcon/weft/ingest"
	pb "github.com/kevlarcon/weft/proto/weftpb"
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

	// Store the configured source under its namespace. Weftd-sourced logs
	// don't have an AWS account (that's cloudwatch-only), so account_id
	// stays nil — NamespaceID carries ownership.
	ns, err := s.store.GetNamespace(customer.ID, namespace)
	if err != nil {
		http.Error(w, "namespace not found: "+err.Error(), http.StatusNotFound)
		return
	}
	if err := s.store.AddLogGroup(
		ns.ID, nil, store.SourceTypeWeftd, fullApp, fullApp,
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

// ── CloudWatch source management (JWT-authenticated) ────────

// handleListSources returns all configured log sources for the caller's
// customer, optionally filtered to a single namespace via ?namespace=X.
// Pulled from AllLogGroups directly (not the coordinator gRPC) because the
// api server owns the store handle and filtering by customer at the DB
// level is trivial.
func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
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

	all, err := s.store.AllLogGroups()
	if err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	nsFilter := r.URL.Query().Get("namespace")

	type sourceResp struct {
		Namespace    string `json:"namespace"`
		App          string `json:"app"`
		SourceType   string `json:"source_type"`
		LogGroup     string `json:"log_group"`
		Region       string `json:"region"`
		PollInterval string `json:"poll_interval"`
		Enabled      bool   `json:"enabled"`
		LastPollAt   string `json:"last_poll_at"`
		AWSAccountID string `json:"aws_account_id,omitempty"`
	}
	out := []sourceResp{}
	for _, lg := range all {
		if lg.CustomerName != customer.Name {
			continue
		}
		if nsFilter != "" && lg.NamespaceName != nsFilter {
			continue
		}
		out = append(out, sourceResp{
			Namespace:    lg.NamespaceName,
			App:          lg.App,
			SourceType:   string(lg.SourceType),
			LogGroup:     lg.LogGroupName,
			Region:       lg.Region,
			PollInterval: lg.PollInterval,
			Enabled:      lg.Enabled,
			LastPollAt:   lg.LastPollAt,
			AWSAccountID: lg.AccountID,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// cloudWatchFormBase carries the fields shared between the sample and
// add-source flows — the creds/log-group identification. Declared once so
// both handlers decode the same JSON shape.
type cloudWatchFormBase struct {
	Namespace    string `json:"namespace"`
	App          string `json:"app"`
	AWSAccountID string `json:"aws_account_id"`
	RoleARN      string `json:"role_arn"`
	ExternalID   string `json:"external_id"`
	LogGroup     string `json:"log_group"`
	Region       string `json:"region"`
}

// handleSampleCloudWatchSource proxies to the coordinator's
// SampleCloudWatchSource RPC so the UI can preview events + a detected
// schema before committing. Requires the same authorization as creating a
// source (see handleAddCloudWatchSource).
func (s *Server) handleSampleCloudWatchSource(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req cloudWatchFormBase
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Namespace == "" || req.LogGroup == "" || req.RoleARN == "" {
		http.Error(w, "namespace, log_group, and role_arn are required", http.StatusBadRequest)
		return
	}

	if err := s.authorizeSourceEdit(claims, req.Namespace); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if s.coordClient == nil {
		http.Error(w, "coordinator unavailable", http.StatusServiceUnavailable)
		return
	}

	resp, err := s.coordClient.SampleCloudWatchSource(s.coordCtx(r), &pb.SampleCloudWatchSourceRequest{
		Namespace:    req.Namespace,
		LogGroup:     req.LogGroup,
		Region:       req.Region,
		AwsAccountId: req.AWSAccountID,
		RoleArn:      req.RoleARN,
		ExternalId:   req.ExternalID,
	})
	if err != nil {
		http.Error(w, "sample failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"lines":     resp.Lines,
		"detection": json.RawMessage(resp.DetectionJson),
	})
}

// handleAddCloudWatchSource is the final step of the add-source flow: the
// user has previewed a sample, confirmed a schema via <SchemaEditor>, and
// is now committing. Builds the Schema YAML server-side from the submitted
// form fields rather than trusting a client-supplied YAML blob.
func (s *Server) handleAddCloudWatchSource(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		cloudWatchFormBase
		PollInterval    string             `json:"poll_interval"`
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
	if req.Namespace == "" || req.App == "" || req.LogGroup == "" || req.RoleARN == "" {
		http.Error(w, "namespace, app, log_group, and role_arn are required", http.StatusBadRequest)
		return
	}

	if err := s.authorizeSourceEdit(claims, req.Namespace); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if s.coordClient == nil {
		http.Error(w, "coordinator unavailable", http.StatusServiceUnavailable)
		return
	}

	fullApp := req.Namespace + "/" + req.App
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
		http.Error(w, "schema marshal: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err := s.coordClient.AddSource(s.coordCtx(r), &pb.AddSourceRequest{
		Namespace:    req.Namespace,
		App:          fullApp,
		LogGroup:     req.LogGroup,
		Region:       req.Region,
		SchemaYaml:   string(schemaYAML),
		PollInterval: req.PollInterval,
		AwsAccountId: req.AWSAccountID,
		RoleArn:      req.RoleARN,
		ExternalId:   req.ExternalID,
	}); err != nil {
		http.Error(w, "add source: "+err.Error(), http.StatusBadGateway)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"status": "configured", "app": fullApp})
}

// handleDeleteSource removes a configured log source. Path shape:
// DELETE /api/sources/{namespace}/{app}. app is the sub-app name; the
// stored log_groups.app is "{namespace}/{app}" so we reconstruct it here.
func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
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

	if err := s.authorizeSourceEdit(claims, namespace); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if s.coordClient == nil {
		http.Error(w, "coordinator unavailable", http.StatusServiceUnavailable)
		return
	}

	fullApp := namespace + "/" + app
	if _, err := s.coordClient.RemoveSource(s.coordCtx(r), &pb.RemoveSourceRequest{
		Namespace: namespace,
		App:       fullApp,
	}); err != nil {
		http.Error(w, "remove source: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// authorizeSourceEdit enforces the same rule as token creation: owners pass
// unconditionally; other users need CanCreateTokens on the namespace. If
// we ever split source permissions from token permissions we'd add a
// dedicated CanManageSources flag — but for now the two tend to travel
// together (both register data ingestion) so reusing the flag is honest.
func (s *Server) authorizeSourceEdit(claims *auth.Claims, namespace string) error {
	if s.isOwner(claims.CustomerName, claims.Email) {
		return nil
	}
	cust, err := s.store.GetCustomerByName(claims.CustomerName)
	if err != nil {
		return errForbidden("customer not found")
	}
	ns, err := s.store.GetNamespace(cust.ID, namespace)
	if err != nil {
		return errForbidden("namespace not found")
	}
	perm, err := s.store.GetNamespacePermission(claims.UserID, ns.ID)
	if err != nil || !perm.CanCreateTokens {
		return errForbidden("no permission to manage sources in this namespace")
	}
	return nil
}

type forbiddenError string

func (e forbiddenError) Error() string { return string(e) }
func errForbidden(msg string) error    { return forbiddenError(msg) }
