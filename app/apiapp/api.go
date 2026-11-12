package apiapp

import (
	"context"
	"flag"
	weftsdk "github.com/w3ft-net/weft-go"
	"log"
	"net/http"
	"os"
	"time"

	weftapi "github.com/graybeardcorp/weft/api"
	"github.com/graybeardcorp/weft/config"
	"github.com/graybeardcorp/weft/ingest"
	ingsyslog "github.com/graybeardcorp/weft/ingest/syslog"
	"github.com/graybeardcorp/weft/store"
)

// API runs the HTTP api process with the deployment a supplies. It returns only
// if the server stops; a start that cannot proceed exits the process, as
// cmd/api always has.
func API(a Adapter) {
	weftsdk.InitLogging("", os.Stderr).CaptureStdio()
	if err := a.validate(); err != nil {
		log.Fatalf("api: %v", err)
	}
	fl := DefineFlags(flag.CommandLine)
	flag.Parse()
	port, dbDSN, coordAddr, baseURL := &fl.Port, &fl.DBDSN, &fl.CoordinatorAddr, &fl.BaseURL
	renderSyslogPort, redisAddr, redisTLS := &fl.RenderSyslogPort, &fl.RedisAddr, &fl.RedisTLS

	jwtSecret := a.JWTSecret()

	dsn := *dbDSN
	if dsn == "" {
		dsn = os.Getenv("DB_DSN")
	}
	if dsn == "" {
		dsn = "host=localhost dbname=weft user=weft password=weft sslmode=disable"
		weftsdk.Warnf(context.Background(), "using default DB DSN — set --db-dsn or DB_DSN in production")
	}

	if err := store.Migrate(store.MigrateDSN(dsn)); err != nil {
		log.Fatalf("migrate database: %v", err)
	}
	s, err := store.Connect(dsn)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer s.Close()

	// Every HTTP ingest request validates its token here and the coordinator
	// validates it again on the forwarded stream; cache the lookup so a warm
	// token costs a map read instead of three SELECTs per request.
	s.EnableAuthCache()

	// Shared Valkey-backed cache for ingest-time schema lookups
	// (api.enrichRow/enrichRows, SyslogServer) — see
	// docs/plans/cloudwatch-kinesis-subscription.md. Same "empty disables"
	// convention as coordinator's --redis-addr for tail fan-out: no flag
	// means every schema lookup goes straight to Postgres, same as
	// before this cache existed. Construction never blocks or errors on
	// an unreachable Redis (store.NewSchemaCache doesn't Ping) — every
	// SchemaCache op is its own short-timeout, best-effort call.
	if *redisAddr != "" {
		s.SetSchemaCache(store.NewSchemaCache(*redisAddr, *redisTLS))
	}

	srv := weftapi.NewServer(s, jwtSecret, *coordAddr, *baseURL)
	if err := srv.Connect(); err != nil {
		log.Fatalf("connect to coordinator: %v", err)
	}
	defer srv.Close()

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The deployment's own background work (hosted: the periodic cleanup of
	// accounts that never completed email verification).
	if a.Start != nil {
		a.Start(ctx, srv)
	}

	// Render TLS syslog listener — see docs/ingestion-syslog.md.
	// weft-nlb terminates TLS against the render.syslog.w3ft.net
	// ACM cert and forwards plaintext TCP here; port 9023 IS the
	// producer identity, so this coordinator process never needs to
	// sniff a connection to know it's Render. A second syslog-speaking
	// platform gets its own SyslogServer on its own port, next to this
	// one, not new code.
	renderSyslog := srv.NewSyslogServer(ingsyslog.RenderProducer{}, *renderSyslogPort)
	if err := renderSyslog.Start(ctx); err != nil {
		log.Fatalf("start render syslog listener: %v", err)
	}

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
