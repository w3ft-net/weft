package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"gorm.io/gorm"

	"github.com/kevlarcon/weft/auth"
	"github.com/kevlarcon/weft/ingest"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/store"
)

// ── Agent-facing endpoints (token-authenticated) ───────────

// handleAgentSample receives sample log lines from weftd for a new source.
// POST /api/agent/sample
//
// UpsertPendingLogGroup is a no-op when a configured row already exists
// for (namespace, app) — the source is approved and the sample is stale.
// That's what prevents the old "source appears as both pending AND
// configured" bug.
func (s *Server) handleAgentSample(w http.ResponseWriter, r *http.Request) {
	agent := GetAgentAuth(r)

	var req struct {
		App        string   `json:"app"`
		Instance   string   `json:"instance"`
		Deployment string   `json:"deployment"`
		Lines      []string `json:"lines"`
		Partial    bool     `json:"partial"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.App == "" {
		http.Error(w, "app required", http.StatusBadRequest)
		return
	}
	if !req.Partial && len(req.Lines) == 0 {
		http.Error(w, "lines required for a final sample", http.StatusBadRequest)
		return
	}

	ns, err := s.store.GetNamespace(agent.CustomerID, agent.Namespace)
	if err != nil {
		http.Error(w, "namespace not found", http.StatusNotFound)
		return
	}

	detection := ingest.Detect(req.Lines)
	sampleJSON, _ := json.Marshal(req.Lines)
	detectionJSON, _ := json.Marshal(detection)

	fullApp := agent.Namespace + "/" + req.App
	if err := s.store.UpsertPendingLogGroup(
		ns.ID, fullApp,
		req.Instance, req.Deployment,
		string(sampleJSON), string(detectionJSON),
		len(req.Lines), req.Partial,
	); err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	respStatus := "pending"
	if req.Partial {
		respStatus = "sampling"
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": respStatus})
}

// handleAgentSourceStatus returns the configuration status of a source.
// GET /api/agent/source-status?app=NAME
func (s *Server) handleAgentSourceStatus(w http.ResponseWriter, r *http.Request) {
	agent := GetAgentAuth(r)

	app := r.URL.Query().Get("app")
	if app == "" {
		http.Error(w, "app parameter required", http.StatusBadRequest)
		return
	}

	lg, err := s.store.FindLogGroup(agent.CustomerID, agent.Namespace, app)
	if err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if lg == nil {
		json.NewEncoder(w).Encode(map[string]any{"status": "unknown"})
		return
	}

	switch lg.Status {
	case store.StatusConfigured:
		json.NewEncoder(w).Encode(map[string]any{
			"status":             "configured",
			"schema":             lg.SchemaYAML,
			"resample_requested": lg.ResampleRequestedAt != nil,
		})
	case store.StatusSampling:
		json.NewEncoder(w).Encode(map[string]any{
			"status":           "sampling",
			"sample_collected": lg.SampleCollected,
			"sample_target":    store.SampleTarget,
		})
	case store.StatusPending:
		json.NewEncoder(w).Encode(map[string]any{
			"status":           "pending",
			"sample_collected": lg.SampleCollected,
			"sample_target":    store.SampleTarget,
		})
	default:
		json.NewEncoder(w).Encode(map[string]any{"status": "unknown"})
	}
}

// ── Dashboard/CLI endpoints (JWT-authenticated) ────────────

// handlePendingSources lists pending sources for the authenticated customer.
// GET /api/sources/pending[?namespace=X]
func (s *Server) handlePendingSources(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)

	customer, err := s.store.GetCustomerByName(session.ActiveCustomerName)
	if err != nil {
		http.Error(w, "customer not found", http.StatusNotFound)
		return
	}

	namespace := r.URL.Query().Get("namespace")
	sources, err := s.store.ListPendingLogGroups(customer.ID, namespace)
	if err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	type pendingResp struct {
		App        string          `json:"app"`
		Namespace  string          `json:"namespace"`
		Instance   string          `json:"instance"`
		Deployment string          `json:"deployment"`
		Sample     json.RawMessage `json:"sample"`
		Detection  json.RawMessage `json:"detection"`
		CreatedAt  string          `json:"created_at"`
		// Status is "sampling" (still accumulating) or "pending" (full
		// sample, ready to configure). SampleCollected/SampleTarget let
		// the UI show "hasn't emitted enough lines yet (N/target)".
		Status          string `json:"status"`
		SampleCollected int    `json:"sample_collected"`
		SampleTarget    int    `json:"sample_target"`
	}

	resp := make([]pendingResp, 0, len(sources))
	for _, lg := range sources {
		// Strip the namespace prefix so the response keeps the same
		// short-form app shape the frontend and weftd agents consume.
		shortApp := strings.TrimPrefix(lg.App, lg.NamespaceName+"/")
		sample := lg.Sample
		if sample == "" {
			sample = "[]"
		}
		detection := lg.Detection
		if detection == "" {
			detection = "null"
		}
		resp = append(resp, pendingResp{
			App:             shortApp,
			Namespace:       lg.NamespaceName,
			Instance:        lg.Instance,
			Deployment:      lg.Deployment,
			Sample:          json.RawMessage(sample),
			Detection:       json.RawMessage(detection),
			CreatedAt:       lg.CreatedAt.Format("2006-01-02T15:04:05Z"),
			Status:          string(lg.Status),
			SampleCollected: lg.SampleCollected,
			SampleTarget:    store.SampleTarget,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleConfigureSource confirms the schema for a pending source.
// POST /api/sources/{namespace}/{app}/configure
func (s *Server) handleConfigureSource(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)

	namespace := r.PathValue("namespace")
	app := r.PathValue("app")
	if namespace == "" || app == "" {
		http.Error(w, "namespace and app required", http.StatusBadRequest)
		return
	}

	customer, err := s.store.GetCustomerByName(session.ActiveCustomerName)
	if err != nil {
		http.Error(w, "customer not found", http.StatusNotFound)
		return
	}

	ns, err := s.store.GetNamespace(customer.ID, namespace)
	if err != nil {
		http.Error(w, "namespace not found", http.StatusNotFound)
		return
	}

	fullApp := namespace + "/" + app
	// A source is configurable when it's pending (first-time config) or
	// already configured (swapping schema after a resample — the row
	// stayed configured and kept ingesting). A still-sampling source has
	// no full sample yet, so it can't be configured.
	lg, err := s.store.FindLogGroup(customer.ID, namespace, app)
	if err != nil {
		log.Printf("configure source %s/%s: FindLogGroup: %v", namespace, app, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if lg == nil {
		http.Error(w, "no source found", http.StatusNotFound)
		return
	}
	if lg.Status == store.StatusSampling {
		http.Error(w, "source is still sampling — not enough lines to configure yet", http.StatusConflict)
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
	schema := &ingest.Schema{
		App:             fullApp,
		Customer:        session.ActiveCustomerName,
		Format:          req.Format,
		TimestampField:  req.TimestampField,
		TimestampFormat: req.TimestampFormat,
		LevelField:      req.LevelField,
		LevelPattern:    req.LevelPattern,
		ExpandKeys:      req.ExpandKeys,
	}

	schemaYAML, err := schema.ToYAML()
	if err != nil {
		log.Printf("configure source %s/%s: schema marshal: %v", namespace, app, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Flip the pending row to configured. The existing (namespace_id, app)
	// row is preserved — only status/schema_yaml/sample/detection change —
	// so there's no window where the source doesn't exist in the table.
	if err := s.store.ConfigureLogGroup(ns.ID, fullApp, string(schemaYAML)); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// The pending row disappeared between GetPendingLogGroup and
			// the UPDATE — either a parallel configure won the race or
			// the source got removed. Surface as 409.
			http.Error(w, "source is no longer pending", http.StatusConflict)
			return
		}
		log.Printf("configure source %s/%s: ConfigureLogGroup: %v", namespace, app, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
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
	session := GetSession(r)
	customer, err := s.store.GetCustomerByName(session.ActiveCustomerName)
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
	session := GetSession(r)

	var req cloudWatchFormBase
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Namespace == "" || req.LogGroup == "" || req.RoleARN == "" {
		http.Error(w, "namespace, log_group, and role_arn are required", http.StatusBadRequest)
		return
	}

	if err := s.authorizeSourceEdit(session, req.Namespace); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
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
	session := GetSession(r)

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

	if err := s.authorizeSourceEdit(session, req.Namespace); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	fullApp := req.Namespace + "/" + req.App
	schema := &ingest.Schema{
		App:             fullApp,
		Customer:        session.ActiveCustomerName,
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
	session := GetSession(r)

	namespace := r.PathValue("namespace")
	app := r.PathValue("app")
	if namespace == "" || app == "" {
		http.Error(w, "namespace and app required", http.StatusBadRequest)
		return
	}

	if err := s.authorizeSourceEdit(session, namespace); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
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

// handleResampleSource asks the agent for a fresh sample of an
// already-configured source. POST /api/sources/{namespace}/{app}/resample
//
// The source stays configured and keeps ingesting on its current schema;
// the agent picks up the flag on its next status poll and re-samples
// concurrently with its tailer. The fresh sample becomes available for
// review (and the source re-appears in the pending list) once the agent
// finishes accumulating.
func (s *Server) handleResampleSource(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)

	namespace := r.PathValue("namespace")
	app := r.PathValue("app")
	if namespace == "" || app == "" {
		http.Error(w, "namespace and app required", http.StatusBadRequest)
		return
	}

	if err := s.authorizeSourceEdit(session, namespace); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	ns, err := s.store.GetNamespace(session.ActiveCustomerID, namespace)
	if err != nil {
		http.Error(w, "namespace not found", http.StatusNotFound)
		return
	}

	fullApp := namespace + "/" + app
	if err := s.store.RequestResample(ns.ID, fullApp); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.Error(w, "source is not configured — nothing to resample", http.StatusConflict)
			return
		}
		log.Printf("resample source %s/%s: RequestResample: %v", namespace, app, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "resample_requested"})
}

// handleSourceSchema returns the current schema (and resample/sampling
// state) for a source. GET /api/sources/{namespace}/{app}/schema
//
// Configured sources return the live schema YAML so it can be viewed at
// any time. A still-sampling source returns its progress so callers can
// say "<source> hasn't emitted enough lines to sample yet (N/target)".
func (s *Server) handleSourceSchema(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)

	namespace := r.PathValue("namespace")
	app := r.PathValue("app")
	if namespace == "" || app == "" {
		http.Error(w, "namespace and app required", http.StatusBadRequest)
		return
	}

	lg, err := s.store.FindLogGroup(session.ActiveCustomerID, namespace, app)
	if err != nil {
		log.Printf("source schema %s/%s: FindLogGroup: %v", namespace, app, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if lg == nil {
		http.Error(w, "no source found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	switch lg.Status {
	case store.StatusConfigured:
		sum := sha256.Sum256([]byte(lg.SchemaYAML))
		resp := map[string]any{
			"status":             "configured",
			"schema_yaml":        lg.SchemaYAML,
			"schema_hash":        hex.EncodeToString(sum[:]),
			"resample_requested": lg.ResampleRequestedAt != nil,
		}
		// While a resample is in flight the source keeps ingesting on
		// its current schema; surface accumulation progress so the UI
		// can show "resample in progress (N/target)".
		if lg.ResampleRequestedAt != nil {
			resp["sample_collected"] = lg.SampleCollected
			resp["sample_target"] = store.SampleTarget
		}
		json.NewEncoder(w).Encode(resp)
	case store.StatusSampling:
		json.NewEncoder(w).Encode(map[string]any{
			"status":           "sampling",
			"sample_collected": lg.SampleCollected,
			"sample_target":    store.SampleTarget,
		})
	default: // pending
		json.NewEncoder(w).Encode(map[string]any{
			"status":           "pending",
			"sample_collected": lg.SampleCollected,
			"sample_target":    store.SampleTarget,
			"detection":        json.RawMessage(lg.Detection),
		})
	}
}

// authorizeSourceEdit enforces the same rule as token creation: owners pass
// unconditionally; other users need CanCreateTokens on the namespace. If
// we ever split source permissions from token permissions we'd add a
// dedicated CanManageSources flag — but for now the two tend to travel
// together (both register data ingestion) so reusing the flag is honest.
func (s *Server) authorizeSourceEdit(session *auth.Session, namespace string) error {
	if session.IsOwner {
		return nil
	}
	ns, err := s.store.GetNamespace(session.ActiveCustomerID, namespace)
	if err != nil {
		return errForbidden("namespace not found")
	}
	perm, err := s.store.GetNamespacePermission(session.UserID, ns.ID)
	if err != nil || !perm.CanCreateTokens {
		return errForbidden("no permission to manage sources in this namespace")
	}
	return nil
}

type forbiddenError string

func (e forbiddenError) Error() string { return string(e) }
func errForbidden(msg string) error    { return forbiddenError(msg) }
