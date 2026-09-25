package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"google.golang.org/grpc/metadata"
	"gopkg.in/yaml.v3"

	"github.com/graybeardcorp/weft/ingest"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
)

// Adapter is the wire-format-specific half of an HTTP ingest endpoint.
// IngestHandler runs the auth half (token lookup, namespace
// prefixing) and forwards parsed rows to the coordinator over the
// existing gRPC IngestStream — the same path weftd uses. An adapter
// declares only how it pulls a token out of the request and how it
// decodes the body into LogRows.
//
// See docs/plans/http-ingest.md for the broader shape (JSON / Heroku /
// CloudWatch all share this interface).
type Adapter interface {
	// Auth pulls the bearer-equivalent secret out of the request.
	// JSONAdapter reads `Authorization: Bearer …`; HerokuAdapter reads
	// the {token} URL path component; CloudWatchAdapter reads
	// `X-Amz-Firehose-Access-Key`.
	Auth(req *http.Request) (string, error)

	// Parse decodes the body into LogRows. Adapters set adapter-known
	// fields (App, Instance, Raw, TsUnixNs, etc.) but never set
	// Customer or Env — those come from the token and are stamped by
	// the coordinator's IngestStream before drain. Adapters MAY set
	// App in their own naming (e.g. HerokuAdapter emits `myapp/web`);
	// IngestHandler prefixes the token's namespace before forwarding,
	// so the coordinator sees `<namespace>/myapp/web` and the existing
	// scope check passes.
	//
	// ac carries the post-auth org/namespace/store access an adapter
	// needs to resolve rows against tenant data mid-parse (e.g.
	// CloudWatchAdapter's logGroup→service lookup) — most adapters
	// (Heroku, JSON) derive everything from the wire format alone and
	// ignore it.
	//
	// The returned ack is opaque to IngestHandler: an adapter that
	// needs a non-default success response (see ResponseWriter) stashes
	// whatever it needs to build that response in ack and gets it back
	// verbatim. Adapters with no custom response return nil.
	Parse(body io.Reader, headers http.Header, ac AdapterContext) (rows []*pb.LogRow, ack any, err error)

	// ContentLimit caps body size pre-decompression.
	ContentLimit() int64
}

// AdapterContext carries the tenant identity IngestHandler has already
// resolved (token → org/namespace) by the time Parse runs, plus store
// access, for adapters that need to resolve rows against tenant data
// mid-parse instead of deriving everything from the wire format.
type AdapterContext struct {
	OrgID     string
	Namespace string
	Store     *store.Store
}

// ResponseWriter is an optional interface an Adapter implements when
// IngestHandler's default response (204 No Content) doesn't match its
// wire protocol — e.g. Firehose's HTTP endpoint delivery requires a 200
// with a JSON body echoing the request's requestId. IngestHandler type-
// asserts for this after a successful Parse + forward and calls it in
// place of the default response, passing back whatever ack Parse
// returned.
type ResponseWriter interface {
	WriteSuccess(w http.ResponseWriter, ack any)
}

// IngestHandler turns an Adapter into an HTTP handler. The flow:
//
//  1. Adapter.Auth → raw token from the request.
//  2. Validate the token against agent_tokens. Fast 401 on miss so
//     malformed drains don't proxy through to the coordinator.
//  3. Parse the body (capped by Adapter.ContentLimit()).
//  4. Prefix every row's App with the token's namespace, so adapter-
//     emitted names like `myapp/web` become `<namespace>/myapp/web`
//     and survive the coordinator's namespace-scope check.
//  5. Open a gRPC IngestStream to the coordinator with the same
//     bearer token in metadata; send rows; close.
//
// Response codes:
//   - 204 on success (Heroku's drain expects 2xx).
//   - 401 on auth failure.
//   - 400 on body-parse failure.
//   - 413 when the body exceeds ContentLimit.
//   - 502 on coordinator-stream failure (transient; client retries).
func (s *Server) IngestHandler(a Adapter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		token, err := a.Auth(req)
		if err != nil {
			http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
			return
		}
		auth, err := s.store.ValidateAgentToken(token)
		if err != nil {
			http.Error(w, "validate token: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if auth == nil || auth.Organization == nil || auth.Organization.ID == "" || auth.Organization.Name == "" {
			http.Error(w, "invalid or revoked token", http.StatusUnauthorized)
			return
		}
		namespace := auth.Namespace
		if namespace == "" {
			http.Error(w, "token has no namespace bound", http.StatusUnauthorized)
			return
		}

		// Cap body before decompression so a malicious POST can't hold
		// a goroutine open for unbounded gunzip work.
		req.Body = http.MaxBytesReader(w, req.Body, a.ContentLimit())

		ac := AdapterContext{OrgID: auth.Organization.ID, Namespace: namespace, Store: s.store}
		rows, ack, err := a.Parse(req.Body, req.Header, ac)
		if err != nil {
			if _, over := err.(*http.MaxBytesError); over {
				http.Error(w, "body exceeds content limit", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "parse: "+err.Error(), http.StatusBadRequest)
			return
		}
		writeSuccess := func() {
			if rw, ok := a.(ResponseWriter); ok {
				rw.WriteSuccess(w, ack)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
		if len(rows) == 0 {
			// Still a successful delivery from the adapter's wire
			// protocol's point of view (e.g. a Firehose CONTROL_MESSAGE
			// health check carries no log events) — let the adapter's
			// own response shape apply even when there's nothing to
			// forward.
			writeSuccess()
			return
		}

		// Namespace is no longer set here — the coordinator stamps it
		// authoritatively from the token on every row. The adapter only
		// supplies service + kind (parsed from the Heroku frame).

		// weftd parses client-side before it ever reaches IngestStream;
		// HTTP-pushed rows arrive here Raw-only (adapters set only what
		// their own wire format hands them for free — Service/Kind/
		// Instance/TsUnixNs). Nothing downstream of this point ever
		// re-parses (IngestStream trusts Level/Fields as sent, by
		// design — see weftd-client-side-parsing.md), so this is the
		// only place an HTTP-pushed row can pick up Level/Fields/
		// TraceId/PayloadJson from its configured schema, the way the
		// CloudWatch poller and weftd already do for their own paths.
		enrichRows(s.store, auth.Organization.ID, namespace, rows)

		// Bootstrap schema detection for any pending kinesis (Firehose
		// push) source still awaiting `weft source configure` — see
		// accumulatePendingKinesisSamples. A no-op for every other
		// adapter/status, so this costs nothing once a source is
		// configured or for adapters that never produce pending rows.
		accumulatePendingKinesisSamples(s.store, s.classifier, auth.Organization.ID, namespace, rows)

		// Forward to coordinator via the existing gRPC IngestStream.
		// Same bearer token in metadata so authenticateStream's path
		// is exactly the path weftd takes — customer/env stamping and
		// SeenServices onboarding happen there, not here.
		ctx := metadata.AppendToOutgoingContext(req.Context(), "authorization", "Bearer "+token)
		stream, err := s.coordClient.IngestStream(ctx)
		if err != nil {
			http.Error(w, "open ingest stream: "+err.Error(), http.StatusBadGateway)
			return
		}
		for _, r := range rows {
			if err := stream.Send(r); err != nil {
				http.Error(w, "send row: "+err.Error(), http.StatusBadGateway)
				return
			}
		}
		if _, err := stream.CloseAndRecv(); err != nil {
			http.Error(w, "close stream: "+err.Error(), http.StatusBadGateway)
			return
		}

		writeSuccess()
	})
}

// resolveParser fetches service's configured schema — cache-aside
// against the shared Valkey schema cache via store.FindLogGroupSchema,
// falling back to Postgres on a miss — and compiles it into a fresh
// *ingest.Parser. ok=false covers "not found," "not configured yet,"
// and "schema failed to parse" alike: callers treat all three the same
// way (skip enrichment, leave the row Raw-only), matching pre-cache
// behavior for any not-yet-onboarded service.
func resolveParser(st *store.Store, orgID, namespace, service string) (*ingest.Parser, bool) {
	schemaYAML, customerName, ok, err := st.FindLogGroupSchema(orgID, namespace, service)
	if err != nil || !ok {
		return nil, false
	}
	var schema ingest.Schema
	if err := yaml.Unmarshal([]byte(schemaYAML), &schema); err != nil {
		return nil, false
	}
	schema.Service = service
	schema.Customer = customerName
	parser, err := ingest.NewParser(&schema)
	if err != nil {
		return nil, false
	}
	return parser, true
}

// applyParsed re-parses row.Raw against parser and overlays only the
// schema-derived fields — Level, Fields, TraceId, PayloadJson, TsUnixNs
// — onto row in place. Service/Kind/Instance set by the adapter/producer
// itself are left alone: those come from the wire format directly (more
// specific than the schema's own generic defaults) and must not be
// clobbered. ingestTime is row's own TsUnixNs, so ParseLine's ingest-
// time fallback (used when the schema has no timestamp field, or
// extraction fails) reproduces exactly what's already on the row —
// safe to copy back unconditionally either way.
func applyParsed(row *pb.LogRow, parser *ingest.Parser) {
	parsed, err := parser.ParseLine(row.Raw, time.Unix(0, row.TsUnixNs))
	if err != nil {
		return
	}
	row.Level = parsed.Level
	row.Fields = parsed.Fields
	row.TraceId = parsed.TraceId
	row.PayloadJson = parsed.PayloadJson
	row.TsUnixNs = parsed.TsUnixNs
}

// enrichRow enriches a single row — SyslogServer's per-frame call site.
// There's no request/batch boundary to locally memoize a parser across
// the way enrichRows does, but store.FindLogGroupSchema's Valkey cache-
// aside is exactly what keeps this cheap across a syslog connection's
// many frames for the same service (a connection can live for hours).
func enrichRow(st *store.Store, orgID, namespace string, row *pb.LogRow) {
	parser, ok := resolveParser(st, orgID, namespace, row.Service)
	if !ok {
		return
	}
	applyParsed(row, parser)
}

// enrichRows enriches a batch — IngestHandler's call site (Heroku, JSON,
// CloudWatch). Adds a per-request memoization layer on top of
// resolveParser so a batch repeating one service many times (a
// CloudWatch Firehose POST spanning one log group's events, a multi-
// frame Heroku drain) doesn't re-hit the Valkey cache once per row.
func enrichRows(st *store.Store, orgID, namespace string, rows []*pb.LogRow) {
	type resolved struct {
		parser *ingest.Parser
		ok     bool
	}
	cache := make(map[string]resolved)
	for _, row := range rows {
		r, hit := cache[row.Service]
		if !hit {
			p, ok := resolveParser(st, orgID, namespace, row.Service)
			r = resolved{parser: p, ok: ok}
			cache[row.Service] = r
		}
		if r.ok {
			applyParsed(row, r.parser)
		}
	}
}

// accumulatePendingKinesisSamples bootstraps schema-detection UX for
// pending kinesis (Firehose push) sources — the CloudWatch-push
// equivalent of weftd's agent-driven SubmitSample/handleAgentSample. A
// customer only registers the AWS-log-group→service mapping up front
// (CreatePendingKinesisSource, via `weft source add --type kinesis`);
// there's no agent to decide when a sample is "done," so this runs on
// every live-ingested batch instead, accumulating raw lines into the
// row's Sample until SampleTarget is reached, exactly mirroring what
// handleAgentSample does per weftd sample submission. See
// docs/plans/cloudwatch-kinesis-subscription.md, "Schema bootstrap."
//
// A full sample does not auto-configure anything — a human still runs
// `weft source configure` (unmodified: ConfigureLogGroup is already
// source-type-agnostic) to approve a schema. A no-op for any row that
// isn't SourceTypeKinesis and still bootstrapping (StatusPending on the
// first call, StatusSampling on every call after — UpsertPendingLogGroup
// sets StatusSampling once `partial` first flips true — so both count):
// weftd's own pending rows never reach this path (its rows arrive via
// IngestStream directly, not IngestHandler/enrichRows), and every other
// source type either commits a schema immediately (poll-mode CloudWatch)
// or has no configure step at all (SourceTypeDirect).
func accumulatePendingKinesisSamples(st *store.Store, classifier ingest.Classifier, orgID, namespace string, rows []*pb.LogRow) {
	byService := make(map[string][]string)
	for _, row := range rows {
		byService[row.Service] = append(byService[row.Service], row.Raw)
	}
	for service, lines := range byService {
		lg, err := st.FindLogGroup(orgID, namespace, service)
		if err != nil || lg == nil {
			continue
		}
		if lg.SourceType != store.SourceTypeKinesis {
			continue
		}
		if lg.Status != store.StatusPending && lg.Status != store.StatusSampling {
			continue
		}
		if lg.SampleCollected >= store.SampleTarget {
			continue
		}
		var existing []string
		if lg.Sample != "" {
			_ = json.Unmarshal([]byte(lg.Sample), &existing)
		}
		combined := append(existing, lines...)
		partial := true
		if len(combined) >= store.SampleTarget {
			combined = combined[:store.SampleTarget]
			partial = false
		}
		sampleJSON, err := json.Marshal(combined)
		if err != nil {
			continue
		}
		namespaceID, svc := lg.NamespaceID, service
		detection := ingest.ClassifyAsync(classifier, combined, !partial, func(d *ingest.Detection) {
			detectionJSON, err := json.Marshal(d)
			if err != nil {
				return
			}
			if err := st.UpdateLogGroupDetection(namespaceID, svc, string(detectionJSON)); err != nil {
				log.Printf("classify: update detection for kinesis source %s/%s: %v", namespace, svc, err)
			}
		})
		detectionJSON, err := json.Marshal(detection)
		if err != nil {
			continue
		}
		_ = st.UpsertPendingLogGroup(lg.NamespaceID, service, "", "",
			string(sampleJSON), string(detectionJSON), len(combined), partial)
	}
}
