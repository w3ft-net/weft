package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	weftapi "github.com/kevlarcon/weft/api"
	"github.com/kevlarcon/weft/config"
	"github.com/kevlarcon/weft/store"
)

func main() {
	port := flag.String("port", "8080", "HTTP listen port")
	dbDSN := flag.String("db-dsn", "", "PostgreSQL DSN")
	coordAddr := flag.String("coordinator", "localhost:9020", "coordinator gRPC address")
	baseURL := flag.String("base-url", "https://w3ft.net", "base URL for magic links")
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

	s, err := store.Open(dsn)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer s.Close()

	srv := weftapi.NewServer(s, jwtSecret, *coordAddr, *baseURL)
	if err := srv.Connect(); err != nil {
		log.Fatalf("connect to coordinator: %v", err)
	}
	defer srv.Close()

	// Configure Stripe prices from env.
	if p := os.Getenv("STRIPE_PRICE_SWATCH"); p != "" {
		srv.StripePrices["swatch"] = p
	}
	if p := os.Getenv("STRIPE_PRICE_YARD"); p != "" {
		srv.StripePrices["yard"] = p
	}
	if p := os.Getenv("STRIPE_PRICE_BOLT"); p != "" {
		srv.StripePrices["bolt"] = p
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
