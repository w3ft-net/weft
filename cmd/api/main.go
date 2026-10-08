package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	weftapi "github.com/graybeardcorp/weft/api"
	"github.com/graybeardcorp/weft/config"
	"github.com/graybeardcorp/weft/ingest"
	ingsyslog "github.com/graybeardcorp/weft/ingest/syslog"
	"github.com/graybeardcorp/weft/store"
)

func main() {
	port := flag.String("port", "8080", "HTTP listen port")
	dbDSN := flag.String("db-dsn", "", "PostgreSQL DSN")
	coordAddr := flag.String("coordinator", "localhost:9020", "coordinator gRPC address")
	baseURL := flag.String("base-url", "https://w3ft.net", "base URL for magic links")
	renderSyslogPort := flag.Int("render-syslog-port", 9023, "TCP port for the Render TLS syslog listener (see docs/ingestion-syslog.md); the NLB terminates TLS and forwards plaintext here")
	redisAddr := flag.String("redis-addr", "", "Redis/Valkey address for the shared ingest-schema cache (host:port); empty disables it, every schema lookup falls through to Postgres directly")
	redisTLS := flag.Bool("redis-tls", false, "use TLS for the Redis connection (required for ElastiCache in-transit encryption)")
	flag.Parse()

	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		jwtSecret = "dev-secret-change-in-production"
		log.Println("WARNING: using default JWT secret — set JWT_SECRET in production")
	}

	dsn := *dbDSN
	if dsn == "" {
		dsn = os.Getenv("DB_DSN")
	}
	if dsn == "" {
		dsn = "host=localhost dbname=weft user=weft password=weft sslmode=disable"
		log.Println("WARNING: using default DB DSN — set --db-dsn or DB_DSN in production")
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
		log.Printf("WARNING: bedrock classifier disabled: %v", err)
	} else if classifier != nil {
		srv.SetClassifier(classifier)
	}

	// Configure Stripe prices from env. Each tier has a flat recurring
	// price plus an optional metered overage price. Absence of the
	// _OVERAGE variant puts that tier in flat-only mode — see
	// docs/plans/stripe.md for the overage-billing plan.
	for _, tier := range []store.Tier{store.TierSwatch, store.TierYard, store.TierBolt} {
		prefix := "STRIPE_PRICE_" + strings.ToUpper(string(tier))
		flat := os.Getenv(prefix)
		if flat == "" {
			continue
		}
		srv.StripePrices[tier] = weftapi.TierPrices{
			Flat:    flat,
			Overage: os.Getenv(prefix + "_OVERAGE"),
		}
	}
	srv.StripeSecretKey = os.Getenv("STRIPE_SECRET_KEY")
	srv.StripePublishableKey = os.Getenv("STRIPE_PUBLISHABLE_KEY")
	srv.StripeWebhookSecret = os.Getenv("STRIPE_WEBHOOK_SECRET")
	srv.SlackClientID = os.Getenv("SLACK_CLIENT_ID")
	srv.SlackClientSecret = os.Getenv("SLACK_CLIENT_SECRET")

	// Start the periodic cleanup of accounts that were created but never
	// completed email verification. See auth.UnverifiedGracePeriod.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.StartUnverifiedSweep(ctx)

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

	// Start the daily Stripe usage reporter. Fires at 03:00 UTC and
	// posts yesterday's ingestion to the metered overage prices for
	// each customer with one. No-op when STRIPE_SECRET_KEY is unset
	// or when a tier has no overage price configured.
	srv.StartUsageReporter(ctx)

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

	log.Printf("weft-api listening on :%s (coordinator: %s)", *port, *coordAddr)
	if err := http.ListenAndServe(":"+*port, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
