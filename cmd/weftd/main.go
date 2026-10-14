package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/weftd"
)

// globalDeployment is set at startup from config and stamped on every LogRow.
// Maps to the "deployment" column on shards (k8s Deployment, AWS ASG, etc.).
var globalDeployment string

// globalRegion is detected once at startup from cloud metadata (or
// supplied via config override) and stamped on every LogRow.
var globalRegion string

// versionReaders maps a per-source `version.file` path to the live
// reader watching it. Built at startup; consulted from the LogRow
// stamping path. Per-source so multi-app hosts can have distinct
// versions. Per-source `version:` is independent of the host-wide
// `dimensions.version:`; the per-source value wins when both are
// configured.
var versionReaders = map[string]*weftd.VersionReader{}

// versionEnvs holds the per-source `version.env` value captured at
// startup. Read once via os.Getenv; no live refresh, by design — env
// vars can't change for a running process anyway. An empty entry (key
// present, value "") means the env was configured but was unset at
// startup.
var versionEnvs = map[string]string{}

// dimensionReaders holds the live FileValueReader per allowed
// column when `dimensions:` is configured. Lookup is by column
// name; missing entries return "" via Current() on a zero reader.
var dimensionReaders = map[string]*weftd.FileValueReader{}

// parserCache holds per-app schema-driven parsers. Populated when
// onboarding completes; consulted on the row-stamping path when
// parseClientSide is enabled.
var parserCache = weftd.NewParserCache()

// resampleSource captures the inputs runSchemaRefresher needs to re-run
// the sampler for an already-configured source when the coordinator
// reports a pending resample. Registered by onboardAndRun (the one place
// that knows a source's path/unit) and keyed by fullApp — the same key
// the parser cache and refresher iterate.
type resampleSource struct {
	path     string // file source; empty for journald
	unit     string // journald source; empty for file
	instance string
}

var (
	resampleMu      sync.Mutex
	resampleSources = map[string]resampleSource{} // fullApp → sampler inputs
	resampleActive  = map[string]bool{}           // fullApp → a resample goroutine is in flight
)

func registerResampleSource(fullApp string, rs resampleSource) {
	resampleMu.Lock()
	resampleSources[fullApp] = rs
	resampleMu.Unlock()
}

// parseClientSide gates the schema-driven parsing path. Default-on as
// of the rollout decision: lines pass through ingest.Parser to extract
// Level / TsUnixNs / PayloadJson before send. Set
// WEFTD_PARSE_CLIENT_SIDE=0 to fall back to shipping rows with raw +
// Fields only (pre-plan behavior) as a rollback lever.
var parseClientSide = os.Getenv("WEFTD_PARSE_CLIENT_SIDE") != "0"

// defaultEndpoint is the production coordinator ingest address. It is the
// only endpoint a real agent ever ships to, so config omits it and weftd
// falls back here. Dockerized/dev setups override by setting endpoint:
// explicitly (e.g. "coordinator:9020").
const defaultEndpoint = "ingest.w3ft.net:9020"

type Config struct {
	Endpoint   string            `yaml:"endpoint"`   // coordinator address; defaults to defaultEndpoint when omitted
	Token      string            `yaml:"token"`      // agent auth token (wft_...)
	Customer   string            `yaml:"customer"`   // fallback if not returned by auth
	Deployment string            `yaml:"deployment"` // fleet/scaling-unit (k8s Deployment, AWS ASG, etc.)
	Region     string            `yaml:"region"`     // optional override; otherwise probed from cloud metadata
	Socket     string            `yaml:"socket"`
	Services   []string          `yaml:"services"`   // logical services co-located on this host; metrics fan out one stream per name
	Dimensions map[string]string `yaml:"dimensions"` // file-sourced dimension values; see docs/plans/dimensions-from-files.md
	Sources    []SourceConfig    `yaml:"sources"`
	AutoUpdate *bool             `yaml:"auto_update"` // self-replace on a newer release; nil = on (set false to opt out)
	Checkpoint CheckpointConfig  `yaml:"checkpoint"`  // file-offset checkpointing; on by default
}

// autoUpdateEnabled resolves the auto_update gate. Unset (nil) means on:
// the fleet self-heals onto new releases by default; an operator opts a
// host out with `auto_update: false` (e.g. to pin a version under
// investigation).
func autoUpdateEnabled(cfg Config) bool {
	return cfg.AutoUpdate == nil || *cfg.AutoUpdate
}

// configError names the reason cfg cannot be run by this binary, or ""
// when it is usable. Kept separate from main so the startup decision
// below is testable without driving os.Exit.
func configError(cfg Config) string {
	if cfg.Endpoint == "" {
		return "endpoint is required"
	}
	if cfg.Token == "" {
		return "token is required: set token: in the config file or WEFTD_TOKEN in the environment"
	}
	return ""
}

// startupDecision is what main does once the config has been parsed and
// the credential chain resolved.
type startupDecision int

const (
	startupRun     startupDecision = iota // config is usable; start the agent
	startupRescued                        // config unusable, but a newer weftd was installed; exit for the restart
	startupFatal                          // config unusable and no newer weftd to rescue us
)

// decideStartup validates cfg and, when this binary cannot run it, gives
// self-update a chance to replace us with one that can.
//
// The ordering matters and is the whole point: an unusable config is
// frequently not a broken config but an *older binary*. Config keys gain
// defaults and get renamed across releases, so a config authored for the
// current weftd can be rejected outright by a stale one — that is how a
// fleet ends up with an agent permanently unable to parse its own
// config. Fataling first made config validation the gate on the very
// mechanism that fixes version skew, turning a solved problem into a
// crash-loop that ships no logs until a human intervenes.
//
// An operator who pinned the host with `auto_update: false` gets the
// fatal they asked for; pinning is usually how someone investigates a
// bad release, so we never climb versions behind their back.
func decideStartup(cfg Config, rescue func() bool) (startupDecision, string) {
	reason := configError(cfg)
	if reason == "" {
		return startupRun, ""
	}
	if autoUpdateEnabled(cfg) && rescue != nil && rescue() {
		return startupRescued, reason
	}
	return startupFatal, reason
}

// allowedDimensionColumns lists the columns that can be populated
// from the `dimensions:` config map. `env`, `app`, `instance`,
// `level`, `ts`, and the k8s-derived columns are intentionally
// excluded — see docs/plans/dimensions-from-files.md for the why.
var allowedDimensionColumns = map[string]bool{
	"deployment": true,
	"version":    true,
	"region":     true,
	"cluster":    true,
	"node":       true,
}

// VersionConfig declares how weftd discovers this source's version
// dimension. Exactly one of `file:` or `env:` should be set; if both
// are, `file:` wins (it's the live-refresh path).
type VersionConfig struct {
	File string `yaml:"file,omitempty"` // path to a file containing the version; fsnotify-watched, refreshed on change
	Env  string `yaml:"env,omitempty"`  // environment-variable name; read once at startup, no live refresh
}

type SourceConfig struct {
	Service string        `yaml:"service,omitempty"` // bare service name within namespace (e.g. "coordinator")
	Kind    string        `yaml:"kind,omitempty"`    // log-stream kind; defaults to "main". Heartbeats/metrics/runtime ship their own kinds.
	Path    string        `yaml:"path"`              // file path or glob (file tailing)
	Unit    string        `yaml:"unit,omitempty"`    // systemd unit name (journal tailing)
	Version VersionConfig `yaml:"version,omitempty"` // how to discover this source's version dimension
	Metrics MetricsConfig `yaml:"metrics,omitempty"` // optional system metrics collection
}

// sourceKind returns the configured kind for a source, defaulting to
// "main". Every tailed log row carries a kind so the pseudofile tree
// is uniform: /log/<ns>/<svc>/<kind>.log. The telemetry streams
// (heartbeats, metrics, runtime) build their own 3-seg app names
// directly and never flow through this.
func sourceKind(src SourceConfig) string {
	if src.Kind != "" {
		return src.Kind
	}
	return "main"
}

// tokenFromChain resolves the auth token. The environment wins over the
// authored file (AWS-shaped credential chain): a non-empty WEFTD_TOKEN
// is authoritative, otherwise the file's token: is used. Token is the
// only value with an env source — see the call site.
func tokenFromChain(envToken, fileToken string) string {
	if envToken != "" {
		return envToken
	}
	return fileToken
}

func main() {
	configPath := flag.String("config", "/etc/weftd/config.yaml", "config file path")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(ClientID())
		os.Exit(0)
	}

	log.Printf("%s starting", ClientID())

	data, err := os.ReadFile(*configPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Fatalf("no config at %s — copy %s.example to it and set your token (see docs/manual/weftd.md)", *configPath, *configPath)
		}
		log.Fatalf("read config: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}

	// Token credential chain (AWS-shaped): WEFTD_TOKEN env wins, else the
	// authored file's token:. Env is the only value with a non-file
	// source — the token is a credential, so a secret injector / config
	// management can supply it without templating the YAML. Nothing
	// structural ever comes from the environment.
	cfg.Token = tokenFromChain(os.Getenv("WEFTD_TOKEN"), cfg.Token)

	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultEndpoint
	}

	// Validate what we need to run — and if we cannot run it, try to
	// climb to a weftd that can before giving up. See decideStartup: a
	// config this binary rejects is often a config written for our
	// successor, and self-update must not sit behind the check that
	// rejects it.
	switch decision, reason := decideStartup(cfg, func() bool {
		return (&weftd.SelfUpdateChecker{
			CurrentVersion: Version,
			AutoApply:      true,
		}).TryUpdateNow()
	}); decision {
	case startupRescued:
		// Restart=always brings the freshly-swapped binary up; it gets a
		// clean parse of the same config.
		log.Printf("config rejected by weftd %s (%s) — updated to a newer build, restarting into it", Version, reason)
		os.Exit(0)
	case startupFatal:
		log.Fatal(reason)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// File-offset checkpointing: resume tailed files where delivery left off.
	// Never fatal; a host that cannot checkpoint tails as it always has.
	flushEvery := configureCheckpointing(cfg.Checkpoint)
	if checkpoints != nil {
		go checkpoints.Run(ctx, flushEvery)
		go runCheckpointStatsReporter(ctx)
	}

	// Background self-update: once a day, fetch the release manifest and,
	// when a newer weftd is published, download + verify + swap the binary
	// and restart into it (systemd Restart=always relaunches). Defaults on
	// so the fleet self-heals onto fixes without an operator touching each
	// host; `auto_update: false` opts a host out and reverts to nag-only.
	go (&weftd.SelfUpdateChecker{
		CurrentVersion: Version,
		AutoApply:      autoUpdateEnabled(cfg),
	}).Run(ctx)

	// Connect to coordinator. The bearer token is attached to every RPC
	// via PerRPCCredentials — IngestStream reads it from metadata.
	coordConn, err := grpc.NewClient(cfg.Endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearerToken{token: cfg.Token, clientID: ClientID()}),
	)
	if err != nil {
		log.Fatalf("connect to coordinator: %v", err)
	}
	coordClient := pb.NewWeftCoordinatorClient(coordConn)

	// Build metrics config to report to coordinator.
	var metricsProto *pb.MetricsConfig
	for _, src := range cfg.Sources {
		if src.Metrics.Enabled {
			collectors := src.Metrics.Collectors
			if len(collectors) == 0 {
				collectors = []string{"cpu", "memory", "disk", "load", "network"}
			}
			metricsProto = &pb.MetricsConfig{
				Enabled:    true,
				Interval:   src.Metrics.Interval,
				Collectors: collectors,
			}
			break // report first enabled source's config
		}
	}

	// Authenticate to verify token and get customer name.
	//   PermissionDenied → subscription inactive; retry in 24h.
	//   Unavailable      → coordinator not reachable yet; retry with backoff.
	//   anything else    → fatal (bad token, bad config).
	var customer, namespace string
	var transportBackoff = time.Second
	for {
		authResp, err := coordClient.Authenticate(ctx, &pb.AuthenticateRequest{
			Token:         cfg.Token,
			MetricsConfig: metricsProto,
		})
		if err != nil {
			switch status.Code(err) {
			case codes.PermissionDenied:
				log.Printf("subscription inactive: %v — retrying in 24h", status.Convert(err).Message())
				select {
				case <-ctx.Done():
					os.Exit(0)
				case <-time.After(24 * time.Hour):
				}
				continue
			case codes.Unavailable:
				log.Printf("coordinator unreachable, retrying in %v: %v", transportBackoff, err)
				select {
				case <-ctx.Done():
					os.Exit(0)
				case <-time.After(transportBackoff):
				}
				transportBackoff *= 2
				if transportBackoff > 30*time.Second {
					transportBackoff = 30 * time.Second
				}
				continue
			default:
				log.Fatalf("authenticate: %v", err)
			}
		}
		customer = authResp.Customer
		if customer == "" {
			customer = cfg.Customer
		}
		namespace = authResp.Namespace
		log.Printf("authenticated as customer=%s namespace=%s", customer, namespace)
		break
	}

	// Periodic re-auth: check subscription status daily.
	// On rejection, log and exit so systemd restarts us into the retry loop above.
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, err := coordClient.Authenticate(ctx, &pb.AuthenticateRequest{Token: cfg.Token})
				if err != nil && status.Code(err) == codes.PermissionDenied {
					log.Fatalf("subscription expired: %v", status.Convert(err).Message())
				}
			}
		}
	}()

	hostname, _ := os.Hostname()
	globalDeployment = cfg.Deployment
	globalRegion = weftd.DetectRegion(ctx, cfg.Region)
	if globalRegion != "" {
		log.Printf("region detected: %s", globalRegion)
	}

	// Initialize file-sourced dimension readers. Each entry validates
	// the column is in the allowed set and the value parses; weftd
	// refuses to start on bad config rather than silently dropping
	// stamps.
	for col, raw := range cfg.Dimensions {
		if !allowedDimensionColumns[col] {
			log.Fatalf("dimensions: column %q is not file-sourceable; allowed: deployment, version, region, cluster, node", col)
		}
		spec, err := weftd.ParseFileValueSpec(raw)
		if err != nil {
			log.Fatalf("dimensions.%s: %v", col, err)
		}
		dimensionReaders[col] = weftd.NewFileValueReader(ctx, spec)
		log.Printf("dimensions.%s: watching %s", col, spec.Path)
	}

	// A concrete file may be matched by a `*.log`-style glob source and
	// also be the target of a dedicated explicit source (e.g. the
	// coordinator's query-audit log, tailed with its own kind). Without
	// deconfliction the file would ingest twice under two different
	// (app,kind) identities. Explicit wins: collect every non-glob
	// `path:` and exclude those concrete files from glob expansion.
	explicitPaths := map[string]bool{}
	for _, src := range cfg.Sources {
		if src.Unit == "" && src.Path != "" && !strings.ContainsAny(src.Path, "*?[") {
			explicitPaths[absClean(src.Path)] = true
		}
	}

	// Start sources — each independently onboards then streams.
	for _, src := range cfg.Sources {
		src := src // capture loop var

		// Derive the service name for this source.
		app := src.Service
		if app == "" && src.Unit != "" {
			app = strings.TrimSuffix(src.Unit, ".service")
		}
		if app == "" && src.Path != "" {
			// Derive from filename: /var/log/weft/coordinator.log → coordinator
			base := filepath.Base(src.Path)
			app = strings.TrimSuffix(base, filepath.Ext(base))
		}
		// Every tailed log lands at <ns>/<svc>/<kind>; kind defaults to "main".
		fullApp := namespace + "/" + app + "/" + sourceKind(src)

		// Per-source version dimension: file path wins (live-refresh
		// via fsnotify), env-var name is the read-once-at-startup
		// fallback. Both empty → the row inherits dimensions.version
		// at row-stamping time, or "" when neither is configured.
		if src.Version.File != "" {
			versionReaders[fullApp] = weftd.NewVersionReader(ctx, src.Version.File)
		} else if src.Version.Env != "" {
			versionEnvs[fullApp] = os.Getenv(src.Version.Env)
		}

		if src.Unit != "" {
			go func() {
				if err := onboardAndRun(ctx, coordClient, cfg.Token, app, src.Path, src.Unit, hostname, fullApp, func() {
					runJournalTailer(ctx, coordClient, hostname, fullApp, src.Unit)
				}); err != nil {
					log.Printf("[%s] onboard failed: %v", fullApp, err)
				}
			}()
		} else if src.Path != "" {
			if strings.ContainsAny(src.Path, "*?[") {
				// Glob: each matched file is onboarded independently from inside watchGlob.
				// The app name derived from the pattern ("*") is meaningless here.
				go watchGlob(ctx, coordClient, cfg.Token, hostname, namespace, sourceKind(src), src.Path, explicitPaths)
			} else {
				// Single file: onboard once, then tail.
				go func() {
					if err := onboardAndRun(ctx, coordClient, cfg.Token, app, src.Path, "", hostname, fullApp, func() {
						runFileTailer(ctx, coordClient, hostname, fullApp, src.Path, true)
					}); err != nil {
						log.Printf("[%s] onboard failed: %v", fullApp, err)
					}
				}()
			}
		}
		if src.Metrics.Enabled {
			go runMetricsCollector(ctx, coordClient, hostname, namespace, effectiveServices(cfg), src.Metrics)
		}
	}

	// Start socket listener.
	if cfg.Socket != "" {
		sock, err := weftd.NewSocketListener(cfg.Socket, "default")
		if err != nil {
			log.Fatalf("socket listener: %v", err)
		}
		defer sock.Close()
		go runSocketIngester(ctx, coordClient, hostname, namespace, sock)
		log.Printf("listening on socket %s", cfg.Socket)
	}

	if parseClientSide {
		go runSchemaRefresher(ctx, coordClient, cfg.Token)
		go runParseStatsReporter(ctx)
	}

	log.Printf("weftd started: %d sources, streaming to %s", len(cfg.Sources), cfg.Endpoint)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
	cancel()
	// Let every file tailer deliver and ack what it has read, then persist
	// the final offsets. Bounded by the shippers' shutdown timeout.
	tailers.shutdown()
	if err := checkpoints.Close(); err != nil {
		log.Printf("checkpoint: final flush: %v", err)
	}
}

// onboardAndRun checks if a source is already configured. If so, runs the
// tailer immediately. If not, submits a sample and polls until configured.
// In both paths, when parseClientSide is on, the schema YAML returned by
// the coordinator is compiled into a per-app parser and stashed in the
// parserCache so the row-stamping hot path can use it.
// unit is non-empty for journald sources (which have no file path); path is
// non-empty for file sources. Exactly one is set.
func onboardAndRun(ctx context.Context, client pb.WeftCoordinatorClient, token, app, path, unit, hostname, fullApp string, run func()) error {
	// Record how to re-sample this source, so the schema refresher can
	// honor a user-requested resample later without disturbing the tailer.
	registerResampleSource(fullApp, resampleSource{path: path, unit: unit, instance: hostname})

	// Check if already configured.
	st, err := weftd.CheckSourceStatus(ctx, client, token, app)
	if err != nil {
		log.Printf("[%s] status check failed: %v — proceeding to sample", fullApp, err)
	} else if st.Status == "configured" {
		installParser(fullApp, st.SchemaYAML)
		log.Printf("[%s] already configured — starting", fullApp)
		run()
		return nil
	}

	// Submit sample — from the unit's journal for journald sources, from
	// the file otherwise.
	if unit != "" {
		log.Printf("[%s] new source — sampling unit %s", fullApp, unit)
		err = weftd.SubmitUnitSample(ctx, client, token, app, unit, hostname, globalDeployment)
	} else {
		log.Printf("[%s] new source — sampling %s", fullApp, path)
		err = weftd.SubmitSample(ctx, client, token, app, path, hostname, globalDeployment)
	}
	if err != nil {
		return err
	}
	log.Printf("[%s] sample submitted — waiting for configuration", fullApp)

	// Poll until configured.
	schemaYAML, err := weftd.WaitForConfiguration(ctx, client, token, app)
	if err != nil {
		return err
	}
	installParser(fullApp, schemaYAML)
	log.Printf("[%s] configured — starting", fullApp)
	run()
	return nil
}

// installParser compiles schemaYAML into a parser for fullApp and
// stashes it in the cache. No-op when parseClientSide is off or the
// YAML is empty (e.g. unconfigured source). Logs and continues on
// compile failures — the row-stamping path falls back to no-parse.
func installParser(fullApp, schemaYAML string) {
	if !parseClientSide || schemaYAML == "" {
		return
	}
	if _, err := parserCache.Install(fullApp, schemaYAML); err != nil {
		log.Printf("[%s] schema install failed, falling back to no-parse: %v", fullApp, err)
	}
}

// runSchemaRefresher polls GetSourceStatus on a 60s ticker for every
// app in the parser cache. When schema_hash differs from the cached
// value, installs a fresh parser; an unchanged hash is a one-RPC no-op.
// A misconfigured source whose status drops back to "pending" stops
// receiving fresh parsers, but the previously installed one keeps
// running until process restart — same posture as today's onboarding.
func runSchemaRefresher(ctx context.Context, client pb.WeftCoordinatorClient, token string) {
	if !parseClientSide {
		return
	}
	const interval = 60 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, fullApp := range parserCache.Apps() {
			refreshSource(ctx, client, token, fullApp)
		}
	}
}

// refreshSource checks one source's status and, if the user asked for a
// resample, kicks it off; otherwise it reinstalls the parser when the
// server's schema hash has moved. The source is looked up by the key the
// coordinator stores it under.
func refreshSource(ctx context.Context, client pb.WeftCoordinatorClient, token, fullApp string) {
	// The coordinator keys log sources on the BARE service —
	// GetSourceStatus → FindLogGroup matches the service column alone and
	// does not strip the kind (unlike SubmitSample). fullApp is the
	// agent's "namespace/service/kind" composite, so pull the service out
	// with splitFullApp; stripping only the namespace prefix would leave
	// "service/kind" and the lookup would never match (every resample and
	// schema refresh silently stranded at "unknown").
	_, service, _ := splitFullApp(fullApp)
	if service == "" {
		return
	}
	st, err := weftd.CheckSourceStatus(ctx, client, token, service)
	if err != nil {
		log.Printf("[%s] schema refresh failed: %v", fullApp, err)
		return
	}
	if st.Status == "configured" && st.ResampleRequested {
		startResample(ctx, client, token, service, fullApp)
	}
	if st.Status != "configured" || st.SchemaYAML == "" {
		return
	}
	cached := parserCache.Slot(fullApp).Hash()
	if cached == st.SchemaHash {
		return
	}
	if _, err := parserCache.Install(fullApp, st.SchemaYAML); err != nil {
		log.Printf("[%s] schema reinstall failed (keeping previous parser): %v", fullApp, err)
		return
	}
	log.Printf("[%s] schema updated (hash %s → %s)", fullApp, shortHash(cached), shortHash(st.SchemaHash))
}

// startResample re-runs the sampler for an already-configured source
// when the user has asked for a fresh sample. The source's tailer keeps
// running — ingestion is not interrupted. The in-flight guard prevents
// stacking a new accumulation every refresh tick while a quiet source is
// still filling its sample (which, by design, can take a while). The
// coordinator clears the resample flag when the final sample lands, so
// subsequent ticks stop triggering this.
func startResample(ctx context.Context, client pb.WeftCoordinatorClient, token, bareService, fullApp string) {
	resampleMu.Lock()
	rs, ok := resampleSources[fullApp]
	if !ok || resampleActive[fullApp] {
		resampleMu.Unlock()
		return
	}
	resampleActive[fullApp] = true
	resampleMu.Unlock()

	log.Printf("[%s] resample requested — sampling concurrently with tailer", fullApp)
	go func() {
		defer func() {
			resampleMu.Lock()
			delete(resampleActive, fullApp)
			resampleMu.Unlock()
		}()
		var err error
		if rs.unit != "" {
			err = weftd.SubmitUnitSample(ctx, client, token, bareService, rs.unit, rs.instance, globalDeployment)
		} else {
			err = weftd.SubmitSample(ctx, client, token, bareService, rs.path, rs.instance, globalDeployment)
		}
		if err != nil {
			log.Printf("[%s] resample failed: %v", fullApp, err)
			return
		}
		log.Printf("[%s] resample complete — fresh sample submitted for review", fullApp)
	}()
}

// shortHash trims a sha256 hex digest down to 8 chars for log lines.
// Empty stays empty (used to render a "(new)" arrow on first install).
func shortHash(h string) string {
	if len(h) < 8 {
		return "(new)"
	}
	return h[:8]
}

// runParseStatsReporter drains the parser cache's per-app counters
// every 60s and emits one log line per app that processed traffic.
// Misconfigured schemas show up as a high missing-level percentage,
// which is the cheapest customer-visible signal we can ship without
// dashboard plumbing. Quiet apps (zero rows in the window) emit no
// line so the log isn't pure noise on idle hosts.
func runParseStatsReporter(ctx context.Context) {
	const interval = 60 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, app := range parserCache.Apps() {
			ok, missing := parserCache.Slot(app).SwapStats()
			total := ok + missing
			if total == 0 {
				continue
			}
			pct := float64(missing) * 100 / float64(total)
			if missing > 0 {
				log.Printf("[%s] parse stats: %d ok, %d missing-level (%.1f%%)", app, ok, missing, pct)
			} else {
				log.Printf("[%s] parse stats: %d ok", app, ok)
			}
		}
	}
}

// bearerToken implements grpc.PerRPCCredentials so every outgoing RPC
// carries `authorization: Bearer <token>` plus `weft-client: <ClientID()>`.
// The coordinator reads the bearer in IngestStream (and any other
// streaming RPC) to validate the caller, and reads weft-client to record
// the agent's version on the token row and gate against a hard-floor
// version.
type bearerToken struct {
	token    string
	clientID string
}

func (b bearerToken) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	md := map[string]string{"authorization": "Bearer " + b.token}
	if b.clientID != "" {
		md["weft-client"] = b.clientID
	}
	return md, nil
}

func (bearerToken) RequireTransportSecurity() bool { return false }

// openStream opens an IngestStream to the coordinator with retry.
// Blocks until a stream is established or ctx is cancelled.
func openStream(ctx context.Context, client pb.WeftCoordinatorClient, label string) (pb.WeftCoordinator_IngestStreamClient, error) {
	var backoff time.Duration = time.Second

	for {
		stream, err := client.IngestStream(ctx)
		if err == nil {
			return stream, nil
		}

		log.Printf("[%s] stream failed, retrying in %v: %v", label, backoff, err)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// runJournalTailer tails a systemd unit's journal and streams lines.
// Reconnects automatically if the stream breaks.
func runJournalTailer(ctx context.Context, client pb.WeftCoordinatorClient, instance, app, unit string) {
	cmd := exec.CommandContext(ctx, "journalctl", "-f", "-u", unit, "--no-pager", "-o", "cat", "-n", "0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[%s] journalctl pipe: %v", app, err)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("[%s] journalctl start: %v", app, err)
		return
	}

	streamScannedLines(ctx, client, bufio.NewScanner(stdout), instance, app)

	cmd.Wait()
}

// streamScannedLines pumps every non-blank line from sc over a SINGLE
// reused IngestStream (reconnecting only when a send breaks), then
// half-closes it on exit. Hoisting the streamState out of the loop is
// the whole point: the previous code passed a fresh &streamState{} per
// line, so each journal line opened its own ingest stream, sent one row,
// and abandoned it — pinning a coordinator handler goroutine in
// stream.Recv() per line (the server-side leak) and churning ~one stream
// per log line. runFileTailer already reused one stream; this brings the
// journal path in line.
func streamScannedLines(ctx context.Context, client pb.WeftCoordinatorClient, sc *bufio.Scanner, instance, app string) {
	ss := &streamState{}
	defer func() {
		if ss.stream != nil {
			// Half-close so the coordinator's handler sees EOF and returns
			// cleanly instead of waiting out the idle reaper.
			ss.stream.CloseAndRecv()
		}
	}()
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if err := sendLineWithReconnect(ctx, client, ss, line, instance, app, ""); err != nil {
			return // context cancelled
		}
	}
}

// fixedPrefix returns the longest leading run of `pattern` that contains
// no glob wildcard. Trailing characters after the last separator are
// dropped so the prefix is always a directory path. For
// "/var/log/*/api.log" returns "/var/log/"; for "/var/log/api.log"
// returns "/var/log/api.log" (no wildcards = whole pattern is fixed);
// for "**/*.log" returns "" (wildcards from the start).
func fixedPrefix(pattern string) string {
	i := strings.IndexAny(pattern, "*?[")
	if i < 0 {
		return pattern
	}
	j := strings.LastIndex(pattern[:i], string(filepath.Separator))
	if j < 0 {
		return ""
	}
	return pattern[:j+1]
}

// matchPath reports whether `path` matches `pattern`, where `**`
// matches zero or more path segments and other wildcards behave as in
// filepath.Match. Pattern and path are split on the OS separator and
// matched segment-by-segment.
func matchPath(pattern, path string) (bool, error) {
	sep := string(filepath.Separator)
	pp := strings.Split(pattern, sep)
	sp := strings.Split(path, sep)
	return matchSegments(pp, sp)
}

func matchSegments(pp, sp []string) (bool, error) {
	if len(pp) == 0 {
		return len(sp) == 0, nil
	}
	if pp[0] == "**" {
		// `**` is greedy across zero or more segments. Try every length.
		for i := 0; i <= len(sp); i++ {
			ok, err := matchSegments(pp[1:], sp[i:])
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}
	if len(sp) == 0 {
		return false, nil
	}
	ok, err := filepath.Match(pp[0], sp[0])
	if err != nil || !ok {
		return ok, err
	}
	return matchSegments(pp[1:], sp[1:])
}

// globMatch returns the file paths matching `pattern`. Unlike
// filepath.Glob, supports `**` for recursive matching. Walking starts
// from fixedPrefix(pattern); a missing prefix dir is not an error
// (returns no matches), matching filepath.Glob's behavior.
func globMatch(pattern string) ([]string, error) {
	if !strings.Contains(pattern, "**") {
		return filepath.Glob(pattern)
	}
	root := fixedPrefix(pattern)
	if root == "" {
		root = "."
	}
	var matches []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return filepath.SkipDir
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		ok, _ := matchPath(pattern, path)
		if ok {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return matches, nil
}

// appFromPath derives the app name for a matched file. It strips the
// fixed prefix from `path`, normalises separators to `/`, and strips
// the file extension. For pattern "*.log" matching "/var/log/api.log"
// returns "api"; for "*/access.log" matching ".../api/access.log"
// returns "api/access"; for "**/*.log" matching ".../v1/access.log"
// returns "v1/access". Path-segment apps round-trip through the
// coordinator's namespace check (it allows any app under the
// authenticated namespace) so the multi-segment form is safe.
func appFromPath(pattern, path string) string {
	prefix := fixedPrefix(pattern)
	rel := strings.TrimPrefix(path, prefix)
	rel = filepath.ToSlash(rel)
	rel = strings.TrimSuffix(rel, filepath.Ext(rel))
	return rel
}

// absClean normalises a path for set-membership comparison between a
// configured explicit `path:` and a glob-matched file. Falls back to a
// lexical clean when the working dir is unavailable so the comparison
// is still consistent (both sides go through the same function).
func absClean(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

// watchGlob watches a directory for new files matching a glob pattern. For
// each matching file — both files present at startup and files created later —
// it runs onboardAndRun with the file's real path and a per-file app name
// derived from the path. Two files that share a basename across different
// subdirectories produce distinct apps (e.g. `*/access.log` matching
// `api/access.log` and `web/access.log` yields apps `api/access` and
// `web/access`). `**` recurses across zero or more path segments.
//
// fsnotify watching is one-level only: it adds the longest fixed prefix
// of the pattern to the watcher and reacts to Create events at that
// depth. Files that appear deeper than the prefix at runtime are not
// auto-discovered — that's the well-known limitation of fsnotify
// without a recursive subscriber. Startup-time discovery via
// globMatch handles the deep case.
func watchGlob(ctx context.Context, client pb.WeftCoordinatorClient, token, instance, namespace, kind, pattern string, excluded map[string]bool) {
	prefix := fixedPrefix(pattern)
	watchDir := strings.TrimSuffix(prefix, string(filepath.Separator))
	if watchDir == "" {
		watchDir = "."
	}
	tailing := make(map[string]bool)

	// startup is false for files created while running: brand new, read from 0.
	startFile := func(p string, startup bool) {
		if tailing[p] {
			return
		}
		if excluded[absClean(p)] {
			log.Printf("[%s] %s claimed by an explicit source — glob skipping", namespace, p)
			return
		}
		tailing[p] = true
		app := appFromPath(pattern, p)
		fullApp := namespace + "/" + app + "/" + kind
		go func() {
			if err := onboardAndRun(ctx, client, token, app, p, "", instance, fullApp, func() {
				runFileTailer(ctx, client, instance, fullApp, p, startup)
			}); err != nil {
				log.Printf("[%s] onboard failed: %v", fullApp, err)
			}
		}()
	}

	// Onboard + tail any files that already match.
	startMatching := func() {
		paths, _ := globMatch(pattern)
		for _, p := range paths {
			startFile(p, true)
		}
	}

	startMatching()
	if len(tailing) > 0 {
		log.Printf("[%s] tailing %d files matching %s", namespace, len(tailing), pattern)
	}

	// Watch directory for new files.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[%s] watch %s: %v", namespace, watchDir, err)
		return
	}
	defer watcher.Close()

	if err := watcher.Add(watchDir); err != nil {
		// Directory doesn't exist yet — wait for it.
		log.Printf("[%s] directory %s not found, waiting...", namespace, watchDir)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := watcher.Add(watchDir); err == nil {
					log.Printf("[%s] directory %s appeared", namespace, watchDir)
					startMatching()
					goto watch
				}
			}
		}
	}

watch:
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Create == fsnotify.Create {
				matched, _ := matchPath(pattern, event.Name)
				if matched {
					log.Printf("[%s] new file %s", namespace, event.Name)
					// Brand new by construction: read from the top so lines written
					// between creation and our open are not lost.
					startFile(event.Name, false)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[%s] watch error: %v", namespace, err)
		}
	}
}

// prefixNamespace stamps the configured namespace on bare app names
// posted to the records socket. SDKs send a logical app like
// "coordinator/runtime"; weftd promotes it to "<ns>/coordinator/runtime"
// so the coordinator's namespace check accepts the row and the
// pseudofile renderer drops it under /log/<ns>/. App names that are
// already qualified pass through untouched.
func prefixNamespace(namespace, app string) string {
	if namespace == "" {
		return app
	}
	if app == namespace || strings.HasPrefix(app, namespace+"/") {
		return app
	}
	return namespace + "/" + app
}

// effectiveServices returns the list of logical services co-located
// on this host. Falls back to a single-element list using deployment
// when `services:` is unset, so existing single-role deploys keep
// working without explicit config.
func effectiveServices(cfg Config) []string {
	if len(cfg.Services) > 0 {
		return cfg.Services
	}
	if cfg.Deployment != "" {
		return []string{cfg.Deployment}
	}
	return nil
}

func runSocketIngester(ctx context.Context, client pb.WeftCoordinatorClient, instance, namespace string, sock *weftd.SocketListener) {
	lines := sock.Lines()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	ss := &streamState{}

	type appLine struct{ app, line, traceID string }
	var buf []appLine

	prefix := func(app string) string { return prefixNamespace(namespace, app) }

	for {
		select {
		case <-ctx.Done():
			for _, al := range buf {
				sendLineWithReconnect(ctx, client, ss, al.line, instance, al.app, al.traceID)
			}
			if ss.stream != nil {
				ss.stream.CloseAndRecv()
			}
			return
		case sl, ok := <-lines:
			if !ok {
				if ss.stream != nil {
					ss.stream.CloseAndRecv()
				}
				return
			}
			buf = append(buf, appLine{app: prefix(sl.App), line: sl.Line, traceID: sl.TraceID})
			if len(buf) >= 100 {
				for _, al := range buf {
					sendLineWithReconnect(ctx, client, ss, al.line, instance, al.app, al.traceID)
				}
				buf = buf[:0]
			}
		case <-ticker.C:
			if len(buf) > 0 {
				for _, al := range buf {
					sendLineWithReconnect(ctx, client, ss, al.line, instance, al.app, al.traceID)
				}
				buf = buf[:0]
			}
		}
	}
}

// streamState holds a lazily-opened, reconnectable IngestStream.
type streamState struct {
	stream pb.WeftCoordinator_IngestStreamClient
}

// versionForService returns the live version dimension for `app`.
// Resolution order: per-source `version.file` (live), per-source
// `version.env` (snapshot at startup), then host-wide
// `dimensions.version`. Lock-free; safe on the hot row-stamping path.
func versionForService(app string) string {
	if r, ok := versionReaders[app]; ok {
		return r.Current()
	}
	if v, ok := versionEnvs[app]; ok {
		return v
	}
	return dimensionValue("version")
}

// dimensionValue reads the latest value for a dimensions-config
// column. Returns "" when the column isn't configured, which lets
// callers compose this with other fallbacks via plain string-OR.
func dimensionValue(col string) string {
	if r, ok := dimensionReaders[col]; ok {
		return r.Current()
	}
	return ""
}

// stampDimensions overlays the configured dimensions: values onto a
// LogRow. Operator config wins over auto-derived sources per the
// resolution order in dimensions-from-files.md.
func stampDimensions(row *pb.LogRow) {
	if v := dimensionValue("deployment"); v != "" {
		row.Deployment = v
	}
	if v := dimensionValue("region"); v != "" {
		row.Region = v
	}
	if v := dimensionValue("cluster"); v != "" {
		row.Cluster = v
	}
	if v := dimensionValue("node"); v != "" {
		row.Node = v
	}
	// `version` is folded into the row at construction via
	// versionForService, which already consults dimensionValue("version")
	// as a fallback. Don't double-stamp here.
}

// buildLogRow constructs a LogRow for `line`. When parseClientSide is on
// and an app-specific parser is installed, the line passes through
// ingest.Parser to extract Level / TsUnixNs / PayloadJson from the
// schema; agent-derived fields (Customer, Instance, Deployment, Region,
// Version) are stamped on top. When no parser is available, falls back
// to the pre-flag behavior: raw + Fields, ingestion-time timestamp,
// empty Level.
// splitFullApp breaks the agent's internal "namespace/service/kind"
// composite back into its parts for the wire. weftd builds the
// composite as namespace + "/" + service + "/" + sourceKind, so this is
// its inverse; it mirrors the coordinator/shard SplitApp defaulting
// (missing kind → "main"). The composite is still the parser-cache key
// and log prefix; only the LogRow carries the split fields now.
func splitFullApp(fullApp string) (namespace, service, kind string) {
	parts := strings.SplitN(fullApp, "/", 3)
	switch len(parts) {
	case 1:
		return parts[0], "", "main"
	case 2:
		return parts[0], parts[1], "main"
	default:
		return parts[0], parts[1], parts[2]
	}
}

func buildLogRow(line, instance, app, traceID string) *pb.LogRow {
	now := time.Now()

	namespace, service, kind := splitFullApp(app)

	if parseClientSide {
		slot := parserCache.Slot(app)
		if parser := slot.Parser(); parser != nil {
			msg, _ := parser.ParseLine(line, now)
			msg.Namespace = namespace
			msg.Service = service
			msg.Kind = kind
			msg.Instance = instance
			msg.Deployment = globalDeployment
			msg.Region = globalRegion
			msg.Version = versionForService(app)
			stampDimensions(msg)
			if slot.HasLevelField() && msg.Level == "" {
				slot.RecordMissingLevel()
			} else {
				slot.RecordOK()
			}
			// A structural (socket-envelope) trace ID is higher-fidelity
			// than schema-based extraction and wins when both are present.
			if traceID != "" {
				msg.TraceId = traceID
			}
			return msg
		}
	}

	row := &pb.LogRow{
		Namespace:  namespace,
		Service:    service,
		Kind:       kind,
		Instance:   instance,
		Deployment: globalDeployment,
		Region:     globalRegion,
		Version:    versionForService(app),
		Raw:        line,
		TsUnixNs:   now.UnixNano(),
		TraceId:    traceID,
	}
	stampDimensions(row)
	return row
}

// sendLineWithReconnect sends a line, opening or reconnecting the stream as needed.
func sendLineWithReconnect(ctx context.Context, client pb.WeftCoordinatorClient, ss *streamState, line, instance, app, traceID string) error {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}

	msg := buildLogRow(line, instance, app, traceID)

	// Try sending on existing stream.
	if ss.stream != nil {
		if err := ss.stream.Send(msg); err == nil {
			return nil
		}
		// Stream is broken — will reconnect below.
		log.Printf("[%s] stream broken, reconnecting", app)
		ss.stream = nil
	}

	// Open a new stream.
	stream, err := openStream(ctx, client, app)
	if err != nil {
		return err // context cancelled
	}
	ss.stream = stream

	// Retry the send on the fresh stream.
	if err := ss.stream.Send(msg); err != nil {
		log.Printf("[%s] send failed after reconnect: %v", app, err)
		ss.stream = nil
	}
	return nil
}

func flushWithReconnect(ctx context.Context, client pb.WeftCoordinatorClient, ss *streamState, lines []string, instance, app string) {
	for _, line := range lines {
		if err := sendLineWithReconnect(ctx, client, ss, line, instance, app, ""); err != nil {
			return
		}
	}
}
