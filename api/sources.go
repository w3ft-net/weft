package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/ingest"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
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

	ns, err := s.store.GetNamespace(agent.OrgID, agent.Namespace)
	if err != nil {
		http.Error(w, "namespace not found", http.StatusNotFound)
		return
	}

	// ClassifyAsync computes the cheap envelope/JSON detection inline and,
	// only for the final (non-partial) sample, kicks off LLM
	// classification in the background — this request returns as soon as
	// the sample itself is stored, never blocked on Bedrock. See
	// ingest/llmdetect.go.
	namespaceID, appName := ns.ID, req.App
	detection := ingest.ClassifyAsync(s.classifier, req.Lines, !req.Partial, func(d *ingest.Detection) {
		detectionJSON, err := json.Marshal(d)
		if err != nil {
			return
		}
		if err := s.store.UpdateLogGroupDetection(namespaceID, appName, string(detectionJSON)); err != nil {
			log.Printf("classify: update detection for %s/%s: %v", agent.Namespace, appName, err)
		}
	})
	sampleJSON, _ := json.Marshal(req.Lines)
	detectionJSON, _ := json.Marshal(detection)

	if err := s.store.UpsertPendingLogGroup(
		ns.ID, req.App,
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

	lg, err := s.store.FindLogGroup(agent.OrgID, agent.Namespace, app)
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

	customer, err := s.store.GetOrgByName(session.ActiveCustomerName)
	if err != nil {
		http.Error(w, "customer not found", http.StatusNotFound)
		return
	}

	namespace := r.URL.Query().Get("namespace")
	var sources []store.LogGroupJoined
	q := s.store.DB().
		Table("log_groups").
		Select(`log_groups.*,
		        organizations.name as customer_name,
		        namespaces.name as namespace_name`).
		Joins("JOIN namespaces ON log_groups.namespace_id = namespaces.id").
		Joins("JOIN organizations ON namespaces.org_id = organizations.id").
		Where("log_groups.status IN ? AND organizations.id = ?",
			[]store.LogGroupStatus{store.StatusSampling, store.StatusPending}, customer.ID)
	if namespace != "" {
		q = q.Where("namespaces.name = ?", namespace)
	}
	err = q.Order("namespaces.name, log_groups.app").Find(&sources).Error
	if err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	type pendingResp struct {
		Service    string          `json:"service"`
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
		// lg.Service is the bare service name; the namespace is carried
		// separately in the response below.
		shortService := lg.Service
		sample := lg.Sample
		if sample == "" {
			sample = "[]"
		}
		detection := lg.Detection
		if detection == "" {
			detection = "null"
		}
		resp = append(resp, pendingResp{
			Service:         shortService,
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

	customer, err := s.store.GetOrgByName(session.ActiveCustomerName)
	if err != nil {
		http.Error(w, "customer not found", http.StatusNotFound)
		return
	}

	ns, err := s.store.GetNamespace(customer.ID, namespace)
	if err != nil {
		http.Error(w, "namespace not found", http.StatusNotFound)
		return
	}

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
		Envelope            string             `json:"envelope"`
		Format              string             `json:"format"`
		FieldSeparator      string             `json:"field_separator"`
		TimestampField      string             `json:"timestamp_field"`
		TimestampFormat     string             `json:"timestamp_format"`
		TimestampFields     int                `json:"timestamp_fields"`
		LevelField          string             `json:"level_field"`
		LevelPattern        string             `json:"level_pattern"`
		TraceIDField        string             `json:"trace_id_field"`
		TraceIDPattern      string             `json:"trace_id_pattern"`
		SpanNameField       string             `json:"span_name_field"`
		SpanNamePattern     string             `json:"span_name_pattern"`
		SpanIDField         string             `json:"span_id_field"`
		SpanIDPattern       string             `json:"span_id_pattern"`
		ParentSpanIDField   string             `json:"parent_span_id_field"`
		ParentSpanIDPattern string             `json:"parent_span_id_pattern"`
		DurationNsField     string             `json:"duration_ns_field"`
		DurationNsPattern   string             `json:"duration_ns_pattern"`
		JSONStartField      int                `json:"json_start_field"`
		ExpandKeys          []ingest.ExpandKey `json:"expand_keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Build schema.
	schema := &ingest.Schema{
		Service:             app,
		Customer:            session.ActiveCustomerName,
		Envelope:            req.Envelope,
		Format:              req.Format,
		FieldSeparator:      req.FieldSeparator,
		TimestampField:      req.TimestampField,
		TimestampFormat:     req.TimestampFormat,
		TimestampFields:     req.TimestampFields,
		LevelField:          req.LevelField,
		LevelPattern:        req.LevelPattern,
		TraceIDField:        req.TraceIDField,
		TraceIDPattern:      req.TraceIDPattern,
		SpanNameField:       req.SpanNameField,
		SpanNamePattern:     req.SpanNamePattern,
		SpanIDField:         req.SpanIDField,
		SpanIDPattern:       req.SpanIDPattern,
		ParentSpanIDField:   req.ParentSpanIDField,
		ParentSpanIDPattern: req.ParentSpanIDPattern,
		DurationNsField:     req.DurationNsField,
		DurationNsPattern:   req.DurationNsPattern,
		JSONStartField:      req.JSONStartField,
		ExpandKeys:          req.ExpandKeys,
	}

	// Defense in depth: the CLI validates before ever sending this
	// request, but the endpoint accepts any authenticated caller (MCP,
	// a hand-rolled script) — parse.go's extractors never error on a
	// bad schema, they just silently extract nothing, so this is the
	// only place a malformed schema gets caught for those callers.
	if problems := schema.Validate(); len(problems) > 0 {
		http.Error(w, "invalid schema:\n"+strings.Join(problems, "\n"), http.StatusBadRequest)
		return
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
	// ConfigureLogGroup also evicts this service's Valkey schema-cache
	// entry (store/schema_cache.go) so every api/coordinator node's next
	// ingest-time lookup picks up the new schema immediately.
	if err := s.store.ConfigureLogGroup(ns.ID, app, string(schemaYAML)); err != nil {
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

// ── Source management (JWT-authenticated) ────────

// handleListSources returns all configured log sources for the caller's
// customer, optionally filtered to a single namespace via ?namespace=X.
// Pulled from AllLogGroups directly (not the coordinator gRPC) because the
// api server owns the store handle and filtering by customer at the DB
// level is trivial.
func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	customer, err := s.store.GetOrgByName(session.ActiveCustomerName)
	if err != nil {
		http.Error(w, "customer not found", http.StatusNotFound)
		return
	}

	all, err := s.store.AllLogGroupsAnyStatus()
	if err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	nsFilter := r.URL.Query().Get("namespace")

	type sourceResp struct {
		Namespace  string `json:"namespace"`
		Service    string `json:"service"`
		Status     string `json:"status"`
		SourceType string `json:"source_type"`
		LogGroup   string `json:"log_group"`
		Region     string `json:"region"`
		Enabled    bool   `json:"enabled"`
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
			Namespace:  lg.NamespaceName,
			Service:    lg.Service,
			Status:     string(lg.Status),
			SourceType: string(lg.SourceType),
			LogGroup:   lg.LogGroupName,
			Region:     lg.Region,
			Enabled:    lg.Enabled,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// handleAddKinesisSource registers the AWS-log-group→service mapping for
// a Firehose push source: no cross-account role to verify and no schema
// to build server-side. The source is created StatusPending; enrichRows/
// accumulatePendingKinesisSamples (api/adapter.go) accumulate a sample
// from the customer's real Firehose deliveries once their CloudFormation
// stack is live, and `weft source configure` (unmodified — already
// source-type-agnostic) finishes onboarding once a human has something
// to approve. See docs/plans/cloudwatch-kinesis-subscription.md.
func (s *Server) handleAddKinesisSource(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)

	var req struct {
		Namespace string `json:"namespace"`
		Service   string `json:"service"`
		LogGroup  string `json:"log_group"`
		Region    string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Namespace == "" || req.Service == "" || req.LogGroup == "" {
		http.Error(w, "namespace, service, and log_group are required", http.StatusBadRequest)
		return
	}

	if err := s.authorizeSourceEdit(session, req.Namespace); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	if _, err := s.coordClient.AddSource(s.coordCtx(r), &pb.AddSourceRequest{
		Namespace: req.Namespace,
		Service:   req.Service,
		LogGroup:  req.LogGroup,
		Region:    req.Region,
	}); err != nil {
		http.Error(w, "add source: "+err.Error(), http.StatusBadGateway)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"status": "pending", "service": req.Service})
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

	if _, err := s.coordClient.RemoveSource(s.coordCtx(r), &pb.RemoveSourceRequest{
		Namespace: namespace,
		Service:   app,
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

	ns, err := s.store.GetNamespace(session.ActiveOrgID, namespace)
	if err != nil {
		http.Error(w, "namespace not found", http.StatusNotFound)
		return
	}

	if err := func() error {
		now := time.Now()
		res := s.store.DB().Model(&store.LogGroup{}).
			Where("namespace_id = ? AND app = ? AND status = ?",
				ns.ID, app, store.StatusConfigured).
			Update("resample_requested_at", now)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	}(); err != nil {
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

	lg, err := s.store.FindLogGroup(session.ActiveOrgID, namespace, app)
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
		// A completed resample leaves a fresh sample on the row (the flag
		// is cleared once the full sample lands). Surfacing it lets a
		// client re-run the wizard and swap the schema — the configure
		// endpoint accepts an already-configured source.
		if lg.Sample != "" && lg.Sample != "[]" {
			resp["sample"] = json.RawMessage(lg.Sample)
			resp["detection"] = json.RawMessage(lg.Detection)
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
		sample := lg.Sample
		if sample == "" {
			sample = "[]"
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status":           "pending",
			"sample_collected": lg.SampleCollected,
			"sample_target":    store.SampleTarget,
			"sample":           json.RawMessage(sample),
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
	ns, err := s.store.GetNamespace(session.ActiveOrgID, namespace)
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
