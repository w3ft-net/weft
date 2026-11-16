package server

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/w3ft-net/weft/alerting"
	weftapi "github.com/w3ft-net/weft/api"
	"github.com/w3ft-net/weft/cmd/api/server/options"
	"github.com/w3ft-net/weft/config"
	"github.com/w3ft-net/weft/ingest"
	ingsyslog "github.com/w3ft-net/weft/ingest/syslog"
	"github.com/w3ft-net/weft/sources"
)

// Flags are the api's command-line flags, parsed.
type Flags = options.Flags

// ParseFlags starts logging, then defines and parses the api's flags. An empty
// --db-dsn is filled from DB_DSN, so Flags.DBDSN is the DSN to open (empty: the
// deployment's own default). Call it before opening anything that logs.
func ParseFlags() *Flags {
	weftsdk.InitLogging("", os.Stderr).CaptureStdio()
	fl := options.Define(flag.CommandLine)
	flag.Parse()
	if fl.DBDSN == "" {
		fl.DBDSN = os.Getenv("DB_DSN")
	}
	return fl
}

// Run runs the HTTP api process over the deployment deps describes, which the
// caller has opened (and will close when Run returns). It returns only if the
// server stops; a start that cannot proceed exits the process, as cmd/api
// always has.
func Run(fl *Flags, deps *Deps) {
	port, coordAddr, baseURL := &fl.Port, &fl.CoordinatorAddr, &fl.BaseURL
	renderSyslogPort := &fl.RenderSyslogPort

	if err := deps.Validate(); err != nil {
		log.Fatalf("api: %v", err)
	}

	srv := weftapi.NewServer(*coordAddr, *baseURL)
	srv.SetIdentity(deps.Identity.Authn)
	if err := srv.Connect(); err != nil {
		log.Fatalf("connect to coordinator: %v", err)
	}
	defer srv.Close()

	srv.SetAccess(deps.Identity.Access)
	if deps.Identity.Groups != nil {
		srv.SetGroups(deps.Identity.Groups)
	}
	if deps.Identity.Entitlements != nil {
		srv.SetEntitlements(deps.Identity.Entitlements)
	}
	b := deps.Backends
	srv.SetTokens(b.Tokens)
	srv.SetSources(b.Sources)
	srv.SetCrons(b.Crons)
	srv.SetBins(b.Bins)
	srv.SetNotify(b.Notify)
	srv.SetIngestFailures(b.IngestFailures)
	srv.SetMounts(deps.Mounts.Config)
	if b.AgentFleet != nil {
		srv.SetAgentFleet(b.AgentFleet)
	}
	ext := deps.Extension
	if ext == nil {
		ext = NopExtension{}
	}
	if targets := ext.AlertTargets(); targets != nil {
		srv.SetAlertKinds(alerting.NewRegistry(srv.NotifyService(), targets...))
	}

	// LLM-based schema-field classification for pending sources — see
	// ingest/llmdetect.go. Disabled (classifier stays nil) unless
	// BEDROCK_CLASSIFIER_MODEL_ID/BEDROCK_REGION are set; that's a
	// deliberate opt-in, not a startup requirement, since it depends on
	// IAM/model-access being provisioned separately (infrastructure-live).
	if classifier, err := ingest.NewBedrockClassifierFromEnv(context.Background()); err != nil {
		weftsdk.Warnf(context.Background(), "WARNING: bedrock classifier disabled: %v", err)
	} else if classifier != nil {
		srv.SetClassifier(classifier)
	}

	// The deployment's own configuration of the server (hosted: Stripe prices
	// and keys, Slack client).
	ext.Configure(srv)
	// The producers the deployment adds, beside the core's. Render's TLS syslog
	// listener is one: it binds the port its producer is given.
	_ = srv.Producers().Register("render", ingsyslog.RenderProducer{ListenPort: *renderSyslogPort})
	ext.Producers(srv.Producers())
	srv.Extend(ext.Routes)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The deployment's own background work (hosted: the periodic cleanup of
	// accounts that never completed email verification).
	ext.Start(ctx, srv)

	// TLS syslog listeners, one per syslog-speaking platform.
	// weft-nlb terminates TLS against the platform's own
	// hostname and ACM cert and forwards plaintext TCP here; the port IS the
	// producer identity, so this process never needs to sniff a connection to
	// know it's Render. A second platform is a producer registered under its own
	// name with its own port, not new code here.
	sources.Each(srv.Producers(), func(name string, p ingsyslog.StreamProducer) {
		ss := srv.NewSyslogServer(p)
		if err := ss.Start(ctx); err != nil {
			log.Fatalf("start %s syslog listener: %v", name, err)
		}
	})

	// The deployment's own work that runs beside the listeners (hosted: the
	// daily Stripe usage reporter).
	ext.Serving(ctx, srv)

	// Kick off the runtime-config watcher. Reads /weft/config/app from
	// SSM on boot and re-reads every 60s, atomically swapping the
	// pointer every rate-limit, body-cap, and grpc-cap consumer holds.
	// Missing param / AWS errors fall through to compile-time defaults.
	appName := os.Getenv("APP_NAME")
	if appName == "" {
		appName = "weft"
	}
	// AWS SDK v2's default chain doesn't reliably resolve region from
	// IMDS; pass it explicitly. AWS_REGION is set by user-data on EC2.
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-2"
	}
	srv.SetConfig(config.Watch(ctx, appName, region, 60*time.Second))

	weftsdk.Infof(ctx, "weft-api listening on :%s (coordinator: %s)", *port, *coordAddr)
	if err := http.ListenAndServe(":"+*port, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
