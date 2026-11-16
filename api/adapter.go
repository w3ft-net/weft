package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	"google.golang.org/grpc/metadata"
	"gopkg.in/yaml.v3"

	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/ingest"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sources"
)

// Adapter is the wire-format-specific half of an HTTP ingest endpoint.
// IngestHandler runs the auth half (token lookup, namespace
// prefixing) and forwards parsed rows to the coordinator over the
// existing gRPC IngestStream — the same path weftd uses. An adapter
// declares only how it pulls a token out of the request and how it
// decodes the body into LogRows.
//
// JSON, Heroku and CloudWatch all share this interface.
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
	// needs to resolve rows against group data mid-parse (e.g.
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

// HTTPProducer is a log producer that delivers over an HTTPS route of the api:
// a producer (the rules of the sources it creates, package sources) that is
// also an Adapter (what it takes to read its requests), mounted at the route it
// names. The registry builds the routes: the api asks it for the producers that
// implement this and mounts each, so adding one is a type and a registration, not
// an edit of the mux.
type HTTPProducer interface {
	sources.Producer
	Adapter
	// Route is the pattern it is mounted at, in http.ServeMux syntax.
	Route() string
}

// DefaultProducers is the registry of the producers the api serves over HTTPS.
// A deployment adds its own through the app's Producers hook. The names are the
// kinds stored on a source row: kinesis is what Firehose's CloudWatch delivery
// has always been called there.
func DefaultProducers() *sources.Registry {
	r := sources.NewRegistry()
	_ = r.Register("heroku", HerokuAdapter{})
	_ = r.Register("kinesis", CloudWatchAdapter{})
	_ = r.Register("otlp", OTLPAdapter{})
	return r
}

// AdapterContext carries the group identity IngestHandler has already
// resolved (token → owner/namespace) by the time Parse runs, plus access to the
// sources, for adapters that need to resolve rows against group data mid-parse
// instead of deriving everything from the wire format.
type AdapterContext struct {
	// Owner is whose token it is, as the group's added columns.
	Owner     shape.Added
	Namespace string
	Sources   source.Store
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
		auth, err := s.tokens.ValidateToken(token)
		if err != nil {
			http.Error(w, "validate token: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if auth == nil {
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

		ac := AdapterContext{Owner: auth.Owner, Namespace: namespace, Sources: s.sources}
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
		// re-parses (IngestStream trusts Level as sent, by design —
		// see weftd-client-side-parsing.md), so this is the only
		// place an HTTP-pushed row can pick up Level/TraceId/
		// PayloadJson from its configured schema, the way the
		// CloudWatch poller and weftd already do for their own paths.
		enrichRows(req.Context(), s.sources, auth.Owner, namespace, rows)

		// Bootstrap schema detection for any push source still awaiting
		// `weft source configure` (pending kinesis, schemaless direct) —
		// see accumulatePendingSamples. A no-op for every other source.
		accumulatePendingSamples(req.Context(), s.sources, s.kindRules(), s.classifier, auth.Owner, namespace, rows)

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
func resolveParser(ctx context.Context, st source.Store, owner shape.Added, namespace string, service string) (*ingest.Parser, bool) {
	schemaYAML, ok, err := st.SourceSchema(ctx, owner, namespace, service)
	if err != nil || !ok {
		return nil, false
	}
	var schema ingest.Schema
	if err := yaml.Unmarshal([]byte(schemaYAML), &schema); err != nil {
		return nil, false
	}
	schema.Service = service
	parser, err := ingest.NewParser(&schema)
	if err != nil {
		return nil, false
	}
	return parser, true
}

// applyParsed re-parses row.Raw against parser and overlays only the
// schema-derived fields — Level, TraceId, PayloadJson, TsUnixNs
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
	row.TraceId = parsed.TraceId
	row.PayloadJson = parsed.PayloadJson
	row.TsUnixNs = parsed.TsUnixNs
}

// enrichRow enriches a single row — SyslogServer's per-frame call site.
// There's no request/batch boundary to locally memoize a parser across
// the way enrichRows does, but store.FindLogGroupSchema's Valkey cache-
// aside is exactly what keeps this cheap across a syslog connection's
// many frames for the same service (a connection can live for hours).
func enrichRow(ctx context.Context, st source.Store, owner shape.Added, namespace string, row *pb.LogRow) {
	parser, ok := resolveParser(ctx, st, owner, namespace, row.Service)
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
func enrichRows(ctx context.Context, st source.Store, owner shape.Added, namespace string, rows []*pb.LogRow) {
	type resolved struct {
		parser *ingest.Parser
		ok     bool
	}
	cache := make(map[string]resolved)
	for _, row := range rows {
		if prebuilt(row) {
			continue
		}
		r, hit := cache[row.Service]
		if !hit {
			p, ok := resolveParser(ctx, st, owner, namespace, row.Service)
			r = resolved{parser: p, ok: ok}
			cache[row.Service] = r
		}
		if r.ok {
			applyParsed(row, r.parser)
		}
	}
}

// prebuilt reports whether an adapter built row's payload itself (an OTLP
// metric, a Heroku l2met line). Schemas are resolved by service, not kind, so
// applying the service's log schema to such a row would re-parse its Raw as a
// log line and overwrite the payload, level and timestamp, and sampling it
// would put metric JSON in the service's log sample.
func prebuilt(row *pb.LogRow) bool { return len(row.PayloadJson) > 0 }

// accumulatePendingSamples bootstraps schema-detection UX for push sources
// that have no agent to sample for them. Two kinds qualify:
//
//   - pending kinesis (Firehose push) sources — the CloudWatch-push
//     equivalent of weftd's agent-driven SubmitSample/handleAgentSample. A
//     customer only registers the AWS-log-group→service mapping up front
//     (RegisterSource, via `weft source add --type kinesis`),
//     so this runs on every live-ingested batch, accumulating raw lines
//     into the row's Sample until SampleTarget is reached, exactly
//     mirroring handleAgentSample.
//   - direct sources (Heroku, Render, bare IngestStream) the coordinator
//     auto-onboarded: already configured and ingesting raw, with a resample
//     requested (store.EnsureServiceLogGroup). The sample fills while rows
//     keep flowing, then surfaces the source for review (store.AwaitingSchema).
//
// A full sample does not auto-configure anything — a human still runs
// `weft source configure` (unmodified: ConfigureLogGroup is already
// source-type-agnostic) to approve a schema. A no-op for every other row:
// weftd's own pending rows never reach this path (its rows arrive via
// IngestStream directly, not IngestHandler/enrichRows), and poll-mode
// CloudWatch commits a schema immediately.
func accumulatePendingSamples(ctx context.Context, st source.Store, producers *sources.Registry, classifier ingest.Classifier, owner shape.Added, namespace string, rows []*pb.LogRow) {
	byService := make(map[string][]string)
	for _, row := range rows {
		if prebuilt(row) {
			continue
		}
		byService[row.Service] = append(byService[row.Service], row.Raw)
	}
	for service, lines := range byService {
		accumulateSample(ctx, st, producers, classifier, owner, namespace, service, lines)
	}
}

// accumulateSample appends lines to service's sample if the service is still
// collecting one, and reports whether it was. SyslogServer calls it per frame
// and uses the answer to stop asking for services that aren't sampling.
func accumulateSample(ctx context.Context, st source.Store, producers *sources.Registry, classifier ingest.Classifier, owner shape.Added, namespace, service string, lines []string) (collecting bool) {
	lg, err := st.FindSource(owner, namespace, service)
	if err != nil {
		return false
	}
	if lg == nil {
		// A new push service has no row yet: the coordinator's provisioner
		// creates it only after these rows are forwarded, which is after
		// this runs. Create it now (the same idempotent call the
		// provisioner makes, so its later one is a no-op) rather than
		// dropping the service's first batch from its sample.
		ns, ok, err := st.GetNamespace(owner, namespace)
		if err != nil || !ok {
			return false
		}
		if err := st.EnsureSource(ns.ID, service); err != nil {
			return false
		}
		if lg, err = st.FindSource(owner, namespace, service); err != nil || lg == nil {
			return false
		}
	}
	// Which sources fill their sample from live rows, and when, is the kind's
	// rule (package sources).
	if !producers.Producer(lg.Kind).CollectsSample(lg.View()) {
		return false
	}
	if lg.SampleCollected >= lg.Target() {
		return false
	}
	var existing []string
	if lg.Sample != "" {
		_ = json.Unmarshal([]byte(lg.Sample), &existing)
	}
	combined := append(existing, lines...)
	partial := true
	if len(combined) >= lg.Target() {
		combined = combined[:lg.Target()]
		partial = false
	}
	sampleJSON, err := json.Marshal(combined)
	if err != nil {
		return false
	}
	namespaceID := lg.NamespaceID
	detection := ingest.ClassifyAsync(ctx, classifier, combined, !partial, func(d *ingest.Detection) {
		detectionJSON, err := json.Marshal(d)
		if err != nil {
			return
		}
		if err := st.UpdateDetection(namespaceID, service, string(sampleJSON), string(detectionJSON)); err != nil {
			weftsdk.Infof(ctx, "classify: update detection for source %s/%s: %v", namespace, service, err)
		}
	})
	detectionJSON, err := json.Marshal(detection)
	if err != nil {
		return false
	}
	if err := st.SubmitSample(namespaceID, service, source.SampleSubmission{
		SampleJSON: string(sampleJSON), DetectionJSON: string(detectionJSON), Collected: len(combined), Partial: partial,
	}); err != nil {
		return false
	}
	return true
}
