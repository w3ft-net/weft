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

// Run runs the HTTP api process with the deployment a supplies. It returns only
// if the server stops; a start that cannot proceed exits the process, as
// cmd/api always has.
func Run[DB any](a Adapter[DB]) {
	weftsdk.InitLogging("", os.Stderr).CaptureStdio()
	fl := options.Define(flag.CommandLine)
	flag.Parse()
	port, dbDSN, coordAddr, baseURL := &fl.Port, &fl.DBDSN, &fl.CoordinatorAddr, &fl.BaseURL
	renderSyslogPort, redisAddr, redisTLS := &fl.RenderSyslogPort, &fl.RedisAddr, &fl.RedisTLS

	if err := a.validate(); err != nil {
		log.Fatalf("api: %v", err)
	}

	dsn := *dbDSN
	if dsn == "" {
		dsn = os.Getenv("DB_DSN")
	}

	s, closeDB, err := a.Open(dsn, *redisAddr, *redisTLS)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer closeDB()

	ident, err := a.Identity(s)
	if err != nil {
		log.Fatalf("api: %v", err)
	}
	srv := weftapi.NewServer(*coordAddr, *baseURL)
	srv.SetIdentity(ident)
	if err := srv.Connect(); err != nil {
		log.Fatalf("connect to coordinator: %v", err)
	}
	defer srv.Close()

	srv.SetTenancy(a.Tenancy(s))
	if a.Tenants != nil {
		srv.SetTenants(a.Tenants)
	}
	if a.Tokens != nil {
		srv.SetTokens(a.Tokens(s))
	}
	if a.Sources != nil {
		srv.SetSources(a.Sources(s))
	}
	if a.Crons != nil {
		srv.SetCrons(a.Crons(s))
	}
	if a.Bins != nil {
		srv.SetBins(a.Bins(s))
	}
	if a.Notify != nil {
		srv.SetNotify(a.Notify(s))
	}
	if a.IngestFailures != nil {
		srv.SetIngestFailures(a.IngestFailures(s))
	}
	if a.Mounts != nil {
		srv.SetMounts(a.Mounts(s))
	}
	if a.Entitlements != nil {
		srv.SetEntitlements(a.Entitlements(s))
	}
	if a.AgentFleet != nil {
		srv.SetAgentFleet(a.AgentFleet(s))
	}
	if a.AlertTargets != nil {
		srv.SetAlertKinds(alerting.NewRegistry(srv.NotifyService(), a.AlertTargets(s)...))
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
	if a.Configure != nil {
		a.Configure(srv)
	}
	// The producers the deployment adds, beside the core's. Render's TLS syslog
	// listener is one: it binds the port its producer is given.
	_ = srv.Producers().Register("render", ingsyslog.RenderProducer{ListenPort: *renderSyslogPort})
	if a.Producers != nil {
		a.Producers(srv.Producers())
	}
	if a.Routes != nil {
		srv.Extend(a.Routes)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The deployment's own background work (hosted: the periodic cleanup of
	// accounts that never completed email verification).
	if a.Start != nil {
		a.Start(ctx, srv)
	}

	// TLS syslog listeners, one per syslog-speaking platform — see
	// docs/ingestion-syslog.md. weft-nlb terminates TLS against the platform's own
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
	if a.Serving != nil {
		a.Serving(ctx, srv)
	}

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
