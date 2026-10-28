package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/ingest"
	"github.com/graybeardcorp/weft/internal/release"
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
	sampleJSON, _ := json.Marshal(req.Lines)
	detection := ingest.ClassifyAsync(r.Context(), s.classifier, req.Lines, !req.Partial, func(d *ingest.Detection) {
		detectionJSON, err := json.Marshal(d)
		if err != nil {
			return
		}
		if err := s.store.UpdateLogGroupDetection(namespaceID, appName, string(sampleJSON), string(detectionJSON)); err != nil {
			weftsdk.Infof(r.Context(), "classify: update detection for %s/%s: %v", agent.Namespace, appName, err)
		}
	})
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
			"sample_target":    lg.Target(),
		})
	case store.StatusPending:
		json.NewEncoder(w).Encode(map[string]any{
			"status":           "pending",
			"sample_collected": lg.SampleCollected,
			"sample_target":    lg.Target(),
		})
	default:
		json.NewEncoder(w).Encode(map[string]any{"status": "unknown"})
	}
}

// ── Dashboard/CLI endpoints (JWT-authenticated) ────────────

// handlePendingSourcesCount returns how many sources await attention. The
// sidebar badge polls this; it needs the number, not the sample blobs.
// GET /api/sources/pending/count
func (s *Server) handlePendingSourcesCount(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)

	n, err := s.store.CountPendingLogGroups(session.ActiveOrgID)
	if err != nil {
		http.Error(w, "store error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"count": n})
}

// handlePendingSources lists pending sources for the authenticated customer.
// GET /api/sources/pending[?namespace=X]
func (s *Server) handlePendingSources(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)

	namespace := r.URL.Query().Get("namespace")
	var sources []store.LogGroupJoined
	q := s.store.DB().
		Table("log_groups").
		Select(`log_groups.*,
		        organizations.name as customer_name,
		        namespaces.name as namespace_name`).
		Joins("JOIN namespaces ON log_groups.namespace_id = namespaces.id").
		Joins("JOIN organizations ON namespaces.org_id = organizations.id").
		Where("organizations.id = ?", session.ActiveOrgID).
		Scopes(store.AwaitingSchema)
	if namespace != "" {
		q = q.Where("namespaces.name = ?", namespace)
	}
	err := q.Order("namespaces.name, log_groups.app").Find(&sources).Error
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
		// SampleUpdatedAt is when the newest sample lines arrived (RFC3339,
		// empty if none yet).
		SampleUpdatedAt string `json:"sample_updated_at"`
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
			Status:          pendingStatus(lg.LogGroup),
			SampleCollected: lg.SampleCollected,
			SampleTarget:    lg.Target(),
			SampleUpdatedAt: formatSampleUpdatedAt(lg.SampleUpdatedAt),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func formatSampleUpdatedAt(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// resampleInfo is a configured source's resample progress: lines collected so
// far, and Ready once the fresh sample is complete and waiting to be
// configured.
type resampleInfo struct {
	Collected int  `json:"collected"`
	Target    int  `json:"target"`
	Ready     bool `json:"ready"`
}

// resampleOf reports the resample state of a configured source with a schema,
// or nil when it has none in flight or waiting. A schemaless source collecting
// its first sample is not a resample.
func resampleOf(lg store.LogGroup) *resampleInfo {
	if lg.Status != store.StatusConfigured || lg.SchemaYAML == "" {
		return nil
	}
	if lg.ResampleRequestedAt != nil {
		return &resampleInfo{Collected: lg.SampleCollected, Target: lg.Target()}
	}
	if lg.Sample != "" && lg.Sample != "[]" && lg.Sample != "null" {
		return &resampleInfo{Collected: lg.SampleCollected, Target: lg.Target(), Ready: true}
	}
	return nil
}

// pendingStatus is the status the pending list reports. A raw-push source
// is live (configured) while its sample fills, so its list status comes
// from sample progress instead of the row's own.
func pendingStatus(lg store.LogGroup) string {
	if lg.Status == store.StatusConfigured {
		// A source with a schema is only listed because of a resample: it is
		// collecting while the flag is set, and ready to configure once the
		// final sample clears it.
		if lg.SchemaYAML != "" {
			if lg.ResampleRequestedAt != nil {
				return string(store.StatusSampling)
			}
			return string(store.StatusPending)
		}
		if lg.SampleCollected < lg.Target() {
			return string(store.StatusSampling)
		}
		return string(store.StatusPending)
	}
	return string(lg.Status)
}

// schemaRequest is the JSON body shared by the configure and
// schema-preview routes: one unsaved ingest.Schema.
type schemaRequest struct {
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
	DurationUnit        string             `json:"duration_unit"`
	JSONStartField      int                `json:"json_start_field"`
	ExpandKeys          []ingest.ExpandKey `json:"expand_keys"`
	// Extract is how each value is read out of its field (what to strip,
	// where to cut, a fallback regex); see ingest.ExtractSpec.
	Extract map[string]ingest.ExtractSpec `json:"extract"`
}

// schema builds the ingest.Schema the request describes for service app.
func (r *schemaRequest) schema(app string, orgID int64) *ingest.Schema {
	return &ingest.Schema{
		Service:             app,
		CustomerID:          orgID,
		Envelope:            r.Envelope,
		Format:              r.Format,
		FieldSeparator:      r.FieldSeparator,
		TimestampField:      r.TimestampField,
		TimestampFormat:     r.TimestampFormat,
		TimestampFields:     r.TimestampFields,
		LevelField:          r.LevelField,
		LevelPattern:        r.LevelPattern,
		TraceIDField:        r.TraceIDField,
		TraceIDPattern:      r.TraceIDPattern,
		SpanNameField:       r.SpanNameField,
		SpanNamePattern:     r.SpanNamePattern,
		SpanIDField:         r.SpanIDField,
		SpanIDPattern:       r.SpanIDPattern,
		ParentSpanIDField:   r.ParentSpanIDField,
		ParentSpanIDPattern: r.ParentSpanIDPattern,
		DurationNsField:     r.DurationNsField,
		DurationNsPattern:   r.DurationNsPattern,
		DurationUnit:        r.DurationUnit,
		JSONStartField:      r.JSONStartField,
		ExpandKeys:          r.ExpandKeys,
		Extract:             r.Extract,
	}
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
		weftsdk.Infof(r.Context(), "configure source %s/%s: FindLogGroup: %v", namespace, app, err)
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

	var req schemaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	schema := req.schema(app, int64(session.ActiveOrgID))

	// Defense in depth: the CLI validates before ever sending this
	// request, but the endpoint accepts any authenticated caller (MCP,
	// a hand-rolled script) — parse.go's extractors never error on a
	// bad schema, they just silently extract nothing, so this is the
	// only place a malformed schema gets caught for those callers.
	if problems := schema.Validate(); len(problems) > 0 {
		http.Error(w, "invalid schema:\n"+strings.Join(problems, "\n"), http.StatusBadRequest)
		return
	}

	// weftd reads the schema on the agent. One that cannot interpret this
	// schema would ignore it silently, so refuse it now, naming what is needed.
	if minAgent := schema.MinAgentVersion(); minAgent != "" && lg.SourceType == store.SourceTypeWeftd {
		if msg := s.agentsCannotReadSchema(r.Context(), lg.ID, minAgent); msg != "" {
			http.Error(w, msg, http.StatusConflict)
			return
		}
	}

	schemaYAML, err := schema.ToYAML()
	if err != nil {
		weftsdk.Infof(r.Context(), "configure source %s/%s: schema marshal: %v", namespace, app, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Flip the pending row to configured. The existing (namespace_id, app)
	// row is preserved — only status/schema_yaml/sample/detection change —
	// so there's no window where the source doesn't exist in the table.
	// ConfigureLogGroup also evicts this service's Valkey schema-cache
	// entry (store/schema_cache.go) so every api/coordinator node's next
	// ingest-time lookup picks up the new schema immediately.
	if err := s.store.ConfigureLogGroup(r.Context(), ns.ID, app, string(schemaYAML)); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// The pending row disappeared between GetPendingLogGroup and
			// the UPDATE — either a parallel configure won the race or
			// the source got removed. Surface as 409.
			http.Error(w, "source is no longer pending", http.StatusConflict)
			return
		}
		weftsdk.Infof(r.Context(), "configure source %s/%s: ConfigureLogGroup: %v", namespace, app, err)
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
		// Agents is who has been reading this source lately: how many hosts
		// reported, and the oldest and newest weftd versions among them.
		// Only weftd sources have agents.
		Agents *agentFleet `json:"agents,omitempty"`
		// Resample is set on a configured source that is being (or has just
		// been) re-sampled: its status stays "configured" because it keeps
		// ingesting, so this is how a list shows the resample at all.
		Resample *resampleInfo `json:"resample,omitempty"`
	}
	out := []sourceResp{}
	var shown []store.LogGroupJoined
	var weftdIDs []uint
	for _, lg := range all {
		if lg.CustomerName != customer.Name {
			continue
		}
		if nsFilter != "" && lg.NamespaceName != nsFilter {
			continue
		}
		shown = append(shown, lg)
		if lg.SourceType == store.SourceTypeWeftd {
			weftdIDs = append(weftdIDs, lg.ID)
		}
	}
	fleets, err := s.store.RecentAgentsFor(weftdIDs, agentWindow)
	if err != nil {
		weftsdk.Infof(r.Context(), "list sources: recent agents: %v", err)
	}
	for _, lg := range shown {
		var fleet *agentFleet
		if agents := fleets[lg.ID]; len(agents) > 0 {
			oldest, n := store.OldestAgentVersion(agents)
			fleet = &agentFleet{Count: n, Oldest: oldest, Newest: store.NewestAgentVersion(agents)}
		}
		out = append(out, sourceResp{
			Agents:     fleet,
			Resample:   resampleOf(lg.LogGroup),
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
// accumulatePendingSamples (api/adapter.go) accumulate a sample
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

	if err := session.AuthorizeNamespaceTokens(s.store, req.Namespace); err != nil {
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

	if err := session.AuthorizeNamespaceTokens(s.store, namespace); err != nil {
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

// handleResampleSource asks for a fresh sample of a pending or
// configured source. POST /api/sources/{namespace}/{app}/resample
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

	if err := session.AuthorizeNamespaceTokens(s.store, namespace); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	ns, err := s.store.GetNamespace(session.ActiveOrgID, namespace)
	if err != nil {
		http.Error(w, "namespace not found", http.StatusNotFound)
		return
	}

	// ?size=N asks for a sample of N lines instead of the default, for a value
	// that appears on only some requests.
	size := 0
	if v := r.URL.Query().Get("size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			http.Error(w, "size must be a number of lines", http.StatusBadRequest)
			return
		}
		size = n
	}

	if err := s.store.RequestResample(ns.ID, app, size); err != nil {
		switch {
		case errors.Is(err, store.ErrBadSampleSize):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, gorm.ErrRecordNotFound):
			http.Error(w, "no source found", http.StatusNotFound)
		case errors.Is(err, store.ErrAlreadySampling):
			http.Error(w, "a sample is already being collected for this source", http.StatusConflict)
		default:
			weftsdk.Infof(r.Context(), "resample source %s/%s: RequestResample: %v", namespace, app, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
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
		weftsdk.Infof(r.Context(), "source schema %s/%s: FindLogGroup: %v", namespace, app, err)
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
			resp["sample_target"] = lg.Target()
			resp["sample_updated_at"] = formatSampleUpdatedAt(lg.SampleUpdatedAt)
		}
		if lg.SchemaYAML != "" {
			if sch, err := ingest.LoadSchemaFromBytes([]byte(lg.SchemaYAML)); err == nil && sch.SDK {
				resp["sdk_note"] = ingest.SDKDerivedNote
			}
			s.addLivePreview(r, session, resp, namespace, app, lg.SchemaYAML)
		}
		json.NewEncoder(w).Encode(resp)
	case store.StatusSampling:
		sample := lg.Sample
		if sample == "" {
			sample = "[]"
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status":            "sampling",
			"sample_collected":  lg.SampleCollected,
			"sample_target":     lg.Target(),
			"sample":            json.RawMessage(sample),
			"sample_updated_at": formatSampleUpdatedAt(lg.SampleUpdatedAt),
		})
	default: // pending
		sample := lg.Sample
		if sample == "" {
			sample = "[]"
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status":            "pending",
			"sample_collected":  lg.SampleCollected,
			"sample_target":     lg.Target(),
			"sample":            json.RawMessage(sample),
			"detection":         json.RawMessage(lg.Detection),
			"sample_updated_at": formatSampleUpdatedAt(lg.SampleUpdatedAt),
		})
	}
}

// agentWindow is how recently an agent must have polled to count as reading a
// source: a host retired weeks ago should not block a schema forever.
const agentWindow = 7 * 24 * time.Hour

// agentsCannotReadSchema says why the agents reading the source cannot be
// trusted with a schema that needs weftd minVersion, or "" if all of them can. An agent
// that has never reported a version (one from before the field existed) counts
// as unknown, not as capable.
func (s *Server) agentsCannotReadSchema(ctx context.Context, logGroupID uint, minVersion string) string {
	agents, err := s.store.RecentAgents(logGroupID, agentWindow)
	if err != nil {
		weftsdk.Infof(ctx, "recent agents for log group %d: %v", logGroupID, err)
		return "could not check which agent versions read this source; try again"
	}
	oldest, n := store.OldestAgentVersion(agents)
	switch {
	case n == 0:
		return fmt.Sprintf("no agent has reported for this source in the last %d days, so its weftd version is unknown; "+
			"this schema needs weftd %s or newer. Upgrade weftd on the hosts shipping this source (they report their version "+
			"as they run), then configure again.", int(agentWindow.Hours()/24), minVersion)
	case release.VersionNewer(minVersion, oldest):
		return fmt.Sprintf("the oldest agent reading this source is weftd %s (%d host(s) reporting); this schema needs weftd %s or newer. "+
			"Upgrade weftd on those hosts, then configure again.", oldest, n, minVersion)
	}
	return ""
}

// agentFleet summarizes the agents reading a source.
type agentFleet struct {
	Count  int    `json:"count"`
	Oldest string `json:"oldest"`
	Newest string `json:"newest"`
}

// schemaPreviewLines is how many lines a schema preview shows.
const schemaPreviewLines = 12

// livePreviewTimeout bounds the coordinator fetch behind a schema GET's preview.
const livePreviewTimeout = 5 * time.Second

// handleSchemaPreview applies an unsaved schema to the head of the source's
// stored sample and returns the lines with every extracted value marked.
// POST /api/sources/{namespace}/{app}/schema-preview — same body as configure.
// It never mutates state, and does not hard-gate on schema.Validate: a
// half-filled schema previews what it can.
func (s *Server) handleSchemaPreview(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	namespace := r.PathValue("namespace")
	app := r.PathValue("app")
	if namespace == "" || app == "" {
		http.Error(w, "namespace and app required", http.StatusBadRequest)
		return
	}

	lg, err := s.store.FindLogGroup(session.ActiveOrgID, namespace, app)
	if err != nil {
		weftsdk.Infof(r.Context(), "schema preview %s/%s: FindLogGroup: %v", namespace, app, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if lg == nil {
		http.Error(w, "no source found", http.StatusNotFound)
		return
	}

	var lines []string
	if lg.Sample != "" {
		if err := json.Unmarshal([]byte(lg.Sample), &lines); err != nil {
			weftsdk.Infof(r.Context(), "schema preview %s/%s: bad sample JSON: %v", namespace, app, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	if len(lines) == 0 {
		http.Error(w, "source has no sample to preview against", http.StatusConflict)
		return
	}
	if len(lines) > schemaPreviewLines {
		lines = lines[:schemaPreviewLines]
	}

	var req schemaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	schema := req.schema(app, int64(session.ActiveOrgID))

	preview, err := ingest.SchemaPreview(schema, lines)
	if err != nil {
		msg := err.Error()
		if problems := schema.Validate(); len(problems) > 0 {
			msg = strings.Join(problems, "\n")
		}
		http.Error(w, "invalid schema:\n"+msg, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(preview)
}

// recentLines returns up to n of the newest raw lines of namespace/app by
// running `tail -n N /log/<ns>/<app>/main.log` through the coordinator, the
// way a shell would, bounded by livePreviewTimeout.
func (s *Server) recentLines(r *http.Request, userID string, namespace, app string, n int) ([]string, error) {
	if s.coordClient == nil {
		return nil, errors.New("coordinator unavailable")
	}
	ctx, cancel := context.WithTimeout(s.coordCtx(r), livePreviewTimeout)
	defer cancel()
	end := time.Now()
	stream, err := s.coordClient.Execute(ctx, &pb.CoordinatorExecuteRequest{
		Pipeline:    fmt.Sprintf("tail -n %d /log/%s/%s/main.log", n, namespace, app),
		TimeStartNs: end.Add(-time.Hour).UnixNano(),
		TimeEndNs:   end.UnixNano(),
	})
	if err != nil {
		return nil, err
	}
	var lines []string
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return lines, nil
		}
		if err != nil {
			return lines, err
		}
		for _, l := range resp.GetLines() {
			lines = append(lines, l.GetRaw())
		}
	}
}

// addLivePreview adds "preview" (the stored schema applied to recent live
// lines) to resp, or "preview_note" saying why there is none. It never fails
// the caller's response.
func (s *Server) addLivePreview(r *http.Request, session *auth.Session, resp map[string]any, namespace, app, schemaYAML string) {
	lines, err := s.recentLines(r, session.UserID, namespace, app, schemaPreviewLines)
	if err != nil && len(lines) == 0 {
		weftsdk.Infof(r.Context(), "source schema %s/%s: live preview fetch: %v", namespace, app, err)
		resp["preview_note"] = "recent lines unavailable: " + err.Error()
		return
	}
	if len(lines) == 0 {
		resp["preview_note"] = "no recent lines"
		return
	}
	if len(lines) > schemaPreviewLines {
		lines = lines[len(lines)-schemaPreviewLines:]
	}
	schema, err := ingest.LoadSchemaFromBytes([]byte(schemaYAML))
	if err != nil {
		resp["preview_note"] = "stored schema unreadable: " + err.Error()
		return
	}
	preview, err := ingest.SchemaPreview(schema, lines)
	if err != nil {
		resp["preview_note"] = "preview failed: " + err.Error()
		return
	}
	resp["preview"] = preview
}
