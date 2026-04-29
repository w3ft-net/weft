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
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/weftd"
)

// globalDeployment is set at startup from config and stamped on every LogRow.
// Maps to the "deployment" column on shards (k8s Deployment, AWS ASG, etc.).
var globalDeployment string

// globalRegion is detected once at startup from cloud metadata (or
// supplied via config override) and stamped on every LogRow.
var globalRegion string

// versionReaders maps a per-source version_file path to the live
// reader watching it. Built at startup; consulted from the LogRow
// stamping path. Per-source so multi-app hosts can have distinct
// versions. Per-source `version_file:` is independent of the
// host-wide `dimensions.version:`; the per-source value wins when
// both are configured.
var versionReaders = map[string]*weftd.VersionReader{}

// dimensionReaders holds the live FileValueReader per allowed
// column when `dimensions:` is configured. Lookup is by column
// name; missing entries return "" via Current() on a zero reader.
var dimensionReaders = map[string]*weftd.FileValueReader{}

// parserCache holds per-app schema-driven parsers. Populated when
// onboarding completes; consulted on the row-stamping path when
// parseClientSide is enabled.
var parserCache = weftd.NewParserCache()

// parseClientSide gates the schema-driven parsing path during rollout.
// When false (default), weftd ships rows with raw + Fields only, same
// behavior as before this plan. When true, lines pass through
// ingest.Parser to extract Level / TsUnixNs / PayloadJson before send.
var parseClientSide = os.Getenv("WEFTD_PARSE_CLIENT_SIDE") == "1"

type Config struct {
	Endpoint   string            `yaml:"endpoint"`   // coordinator address
	Token      string            `yaml:"token"`      // agent auth token (wft_...)
	Customer   string            `yaml:"customer"`   // fallback if not returned by auth
	Deployment string            `yaml:"deployment"` // fleet/scaling-unit (k8s Deployment, AWS ASG, etc.)
	Region     string            `yaml:"region"`     // optional override; otherwise probed from cloud metadata
	Socket     string            `yaml:"socket"`
	Services   []string          `yaml:"services"`   // logical services co-located on this host; metrics + heartbeats fan out one stream per name
	Dimensions map[string]string `yaml:"dimensions"` // file-sourced dimension values; see docs/plans/dimensions-from-files.md
	Heartbeat  HeartbeatConfig   `yaml:"heartbeat"`  // local app-heartbeat listener
	Sources    []SourceConfig    `yaml:"sources"`
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

type SourceConfig struct {
	App         string        `yaml:"app,omitempty"`          // bare app name within namespace (e.g. "coordinator")
	Path        string        `yaml:"path"`                   // file path or glob (file tailing)
	Unit        string        `yaml:"unit,omitempty"`         // systemd unit name (journal tailing)
	VersionFile string        `yaml:"version_file,omitempty"` // path to a file containing this source's version; fsnotify-watched
	Metrics     MetricsConfig `yaml:"metrics,omitempty"`      // optional system metrics collection
}

func main() {
	configPath := flag.String("config", "/etc/weftd/config.yaml", "config file path")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("weftd " + Version)
		os.Exit(0)
	}

	log.Printf("weftd %s starting", Version)

	data, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}

	if cfg.Endpoint == "" {
		log.Fatal("endpoint is required")
	}
	if cfg.Token == "" {
		log.Fatal("token is required")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Connect to coordinator. The bearer token is attached to every RPC
	// via PerRPCCredentials — IngestStream reads it from metadata.
	coordConn, err := grpc.NewClient(cfg.Endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(bearerToken{token: cfg.Token}),
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

	// Start sources — each independently onboards then streams.
	for _, src := range cfg.Sources {
		src := src // capture loop var

		// Derive the app name for this source.
		app := src.App
		if app == "" && src.Unit != "" {
			app = strings.TrimSuffix(src.Unit, ".service")
		}
		if app == "" && src.Path != "" {
			// Derive from filename: /var/log/weft/coordinator.log → coordinator
			base := filepath.Base(src.Path)
			app = strings.TrimSuffix(base, filepath.Ext(base))
		}
		fullApp := namespace + "/" + app

		// If this source declares a version_file, spin up a reader so
		// row-stamping can fetch the current version cheaply.
		if src.VersionFile != "" {
			versionReaders[fullApp] = weftd.NewVersionReader(ctx, src.VersionFile)
		}

		if src.Unit != "" {
			go func() {
				if err := onboardAndRun(ctx, coordClient, cfg.Token, customer, app, src.Path, hostname, fullApp, func() {
					runJournalTailer(ctx, coordClient, customer, hostname, fullApp, src.Unit)
				}); err != nil {
					log.Printf("[%s] onboard failed: %v", fullApp, err)
				}
			}()
		} else if src.Path != "" {
			if strings.ContainsAny(src.Path, "*?[") {
				// Glob: each matched file is onboarded independently from inside watchGlob.
				// The app name derived from the pattern ("*") is meaningless here.
				go watchGlob(ctx, coordClient, cfg.Token, customer, hostname, namespace, src.Path)
			} else {
				// Single file: onboard once, then tail.
				go func() {
					if err := onboardAndRun(ctx, coordClient, cfg.Token, customer, app, src.Path, hostname, fullApp, func() {
						runFileTailer(ctx, coordClient, customer, hostname, fullApp, src.Path)
					}); err != nil {
						log.Printf("[%s] onboard failed: %v", fullApp, err)
					}
				}()
			}
		}
		if src.Metrics.Enabled {
			go runMetricsCollector(ctx, coordClient, customer, hostname, namespace, effectiveServices(cfg), src.Metrics)
		}
	}

	// Start socket listener.
	if cfg.Socket != "" {
		sock, err := weftd.NewSocketListener(cfg.Socket, "default")
		if err != nil {
			log.Fatalf("socket listener: %v", err)
		}
		defer sock.Close()
		go runSocketIngester(ctx, coordClient, customer, hostname, namespace, sock)
		log.Printf("listening on socket %s", cfg.Socket)
	}

	// Start the app-heartbeat listener and snapshot ticker. Always
	// on by default with a Unix socket; opt-in for the TCP listener.
	if _, err := startHeartbeat(ctx, cfg.Heartbeat, coordClient, customer, hostname, namespace); err != nil {
		log.Printf("heartbeat: disabled (%v)", err)
	}

	if parseClientSide {
		go runSchemaRefresher(ctx, coordClient, cfg.Token, customer, namespace)
		go runParseStatsReporter(ctx)
	}

	log.Printf("weftd started: %d sources, streaming to %s", len(cfg.Sources), cfg.Endpoint)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
	cancel()
}

// onboardAndRun checks if a source is already configured. If so, runs the
// tailer immediately. If not, submits a sample and polls until configured.
// In both paths, when parseClientSide is on, the schema YAML returned by
// the coordinator is compiled into a per-app parser and stashed in the
// parserCache so the row-stamping hot path can use it.
func onboardAndRun(ctx context.Context, client pb.WeftCoordinatorClient, token, customer, app, path, hostname, fullApp string, run func()) error {
	// Check if already configured.
	st, err := weftd.CheckSourceStatus(ctx, client, token, app)
	if err != nil {
		log.Printf("[%s] status check failed: %v — proceeding to sample", fullApp, err)
	} else if st.Status == "configured" {
		installParser(fullApp, customer, st.SchemaYAML)
		log.Printf("[%s] already configured — starting", fullApp)
		run()
		return nil
	}

	// Submit sample.
	log.Printf("[%s] new source — sampling %s", fullApp, path)
	if err := weftd.SubmitSample(ctx, client, token, app, path, hostname, globalDeployment); err != nil {
		return err
	}
	log.Printf("[%s] sample submitted — waiting for configuration", fullApp)

	// Poll until configured.
	schemaYAML, err := weftd.WaitForConfiguration(ctx, client, token, app)
	if err != nil {
		return err
	}
	installParser(fullApp, customer, schemaYAML)
	log.Printf("[%s] configured — starting", fullApp)
	run()
	return nil
}

// installParser compiles schemaYAML into a parser for fullApp and
// stashes it in the cache. No-op when parseClientSide is off or the
// YAML is empty (e.g. unconfigured source). Logs and continues on
// compile failures — the row-stamping path falls back to no-parse.
func installParser(fullApp, customer, schemaYAML string) {
	if !parseClientSide || schemaYAML == "" {
		return
	}
	if _, err := parserCache.Install(fullApp, customer, schemaYAML); err != nil {
		log.Printf("[%s] schema install failed, falling back to no-parse: %v", fullApp, err)
	}
}

// runSchemaRefresher polls GetSourceStatus on a 60s ticker for every
// app in the parser cache. When schema_hash differs from the cached
// value, installs a fresh parser; an unchanged hash is a one-RPC no-op.
// A misconfigured source whose status drops back to "pending" stops
// receiving fresh parsers, but the previously installed one keeps
// running until process restart — same posture as today's onboarding.
func runSchemaRefresher(ctx context.Context, client pb.WeftCoordinatorClient, token, customer, namespace string) {
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
			bareApp := strings.TrimPrefix(fullApp, namespace+"/")
			if bareApp == fullApp {
				continue
			}
			st, err := weftd.CheckSourceStatus(ctx, client, token, bareApp)
			if err != nil {
				log.Printf("[%s] schema refresh failed: %v", fullApp, err)
				continue
			}
			if st.Status != "configured" || st.SchemaYAML == "" {
				continue
			}
			cached := parserCache.Slot(fullApp).Hash()
			if cached == st.SchemaHash {
				continue
			}
			if _, err := parserCache.Install(fullApp, customer, st.SchemaYAML); err != nil {
				log.Printf("[%s] schema reinstall failed (keeping previous parser): %v", fullApp, err)
				continue
			}
			log.Printf("[%s] schema updated (hash %s → %s)", fullApp, shortHash(cached), shortHash(st.SchemaHash))
		}
	}
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
// carries `authorization: Bearer <token>`. The coordinator reads this in
// IngestStream (and any other streaming RPC) to validate the caller.
type bearerToken struct{ token string }

func (b bearerToken) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
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
func runJournalTailer(ctx context.Context, client pb.WeftCoordinatorClient, customer, instance, app, unit string) {
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

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if err := sendLineWithReconnect(ctx, client, &streamState{}, line, customer, instance, app); err != nil {
			return // context cancelled
		}
	}

	cmd.Wait()
}

func runFileTailer(ctx context.Context, client pb.WeftCoordinatorClient, customer, instance, app, path string) {
	tailer, err := weftd.NewTailer(path)
	if err != nil {
		log.Printf("[%s] open %s: %v", app, path, err)
		return
	}
	defer tailer.Close()

	lines := tailer.Lines()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	ss := &streamState{}
	var buf []string

	for {
		select {
		case <-ctx.Done():
			flushWithReconnect(ctx, client, ss, buf, customer, instance, app)
			if ss.stream != nil {
				ss.stream.CloseAndRecv()
			}
			return
		case line, ok := <-lines:
			if !ok {
				flushWithReconnect(ctx, client, ss, buf, customer, instance, app)
				if ss.stream != nil {
					ss.stream.CloseAndRecv()
				}
				return
			}
			buf = append(buf, line)
			if len(buf) >= 100 {
				flushWithReconnect(ctx, client, ss, buf, customer, instance, app)
				buf = buf[:0]
			}
		case <-ticker.C:
			if len(buf) > 0 {
				flushWithReconnect(ctx, client, ss, buf, customer, instance, app)
				buf = buf[:0]
			}
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
func watchGlob(ctx context.Context, client pb.WeftCoordinatorClient, token, customer, instance, namespace, pattern string) {
	prefix := fixedPrefix(pattern)
	watchDir := strings.TrimSuffix(prefix, string(filepath.Separator))
	if watchDir == "" {
		watchDir = "."
	}
	tailing := make(map[string]bool)

	startFile := func(p string) {
		if tailing[p] {
			return
		}
		tailing[p] = true
		app := appFromPath(pattern, p)
		fullApp := namespace + "/" + app
		go func() {
			if err := onboardAndRun(ctx, client, token, customer, app, p, instance, fullApp, func() {
				runFileTailer(ctx, client, customer, instance, fullApp, p)
			}); err != nil {
				log.Printf("[%s] onboard failed: %v", fullApp, err)
			}
		}()
	}

	// Onboard + tail any files that already match.
	startMatching := func() {
		paths, _ := globMatch(pattern)
		for _, p := range paths {
			startFile(p)
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
					startFile(event.Name)
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

func runSocketIngester(ctx context.Context, client pb.WeftCoordinatorClient, customer, instance, namespace string, sock *weftd.SocketListener) {
	lines := sock.Lines()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	ss := &streamState{}

	type appLine struct{ app, line string }
	var buf []appLine

	prefix := func(app string) string { return prefixNamespace(namespace, app) }

	for {
		select {
		case <-ctx.Done():
			for _, al := range buf {
				sendLineWithReconnect(ctx, client, ss, al.line, customer, instance, al.app)
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
			buf = append(buf, appLine{app: prefix(sl.App), line: sl.Line})
			if len(buf) >= 100 {
				for _, al := range buf {
					sendLineWithReconnect(ctx, client, ss, al.line, customer, instance, al.app)
				}
				buf = buf[:0]
			}
		case <-ticker.C:
			if len(buf) > 0 {
				for _, al := range buf {
					sendLineWithReconnect(ctx, client, ss, al.line, customer, instance, al.app)
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

// versionForApp returns the live version dimension for `app`. Falls
// back to dimensions.version when no per-source version_file is
// configured. Lock-free; safe on the hot row-stamping path.
func versionForApp(app string) string {
	if r, ok := versionReaders[app]; ok {
		return r.Current()
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
	// versionForApp, which already consults dimensionValue("version")
	// as a fallback. Don't double-stamp here.
}

// buildLogRow constructs a LogRow for `line`. When parseClientSide is on
// and an app-specific parser is installed, the line passes through
// ingest.Parser to extract Level / TsUnixNs / PayloadJson from the
// schema; agent-derived fields (Customer, Instance, Deployment, Region,
// Version) are stamped on top. When no parser is available, falls back
// to the pre-flag behavior: raw + Fields, ingestion-time timestamp,
// empty Level.
func buildLogRow(line, customer, instance, app string) *pb.LogRow {
	now := time.Now()

	if parseClientSide {
		slot := parserCache.Slot(app)
		if parser := slot.Parser(); parser != nil {
			msg, _ := parser.ParseLine(line, now)
			msg.App = app
			msg.Customer = customer
			msg.Instance = instance
			msg.Deployment = globalDeployment
			msg.Region = globalRegion
			msg.Version = versionForApp(app)
			stampDimensions(msg)
			if slot.HasLevelField() && msg.Level == "" {
				slot.RecordMissingLevel()
			} else {
				slot.RecordOK()
			}
			return msg
		}
	}

	row := &pb.LogRow{
		App:        app,
		Customer:   customer,
		Instance:   instance,
		Deployment: globalDeployment,
		Region:     globalRegion,
		Version:    versionForApp(app),
		Raw:        line,
		TsUnixNs:   now.UnixNano(),
		Fields:     strings.Fields(line),
	}
	stampDimensions(row)
	return row
}

// sendLineWithReconnect sends a line, opening or reconnecting the stream as needed.
func sendLineWithReconnect(ctx context.Context, client pb.WeftCoordinatorClient, ss *streamState, line, customer, instance, app string) error {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}

	msg := buildLogRow(line, customer, instance, app)

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

func flushWithReconnect(ctx context.Context, client pb.WeftCoordinatorClient, ss *streamState, lines []string, customer, instance, app string) {
	for _, line := range lines {
		if err := sendLineWithReconnect(ctx, client, ss, line, customer, instance, app); err != nil {
			return
		}
	}
}
