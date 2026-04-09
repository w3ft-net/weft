package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	weftapi "github.com/kevlarcon/weft/api"
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
		dsn = os.Getenv("WEFT_DB_DSN")
	}
	if dsn == "" {
		dsn = "host=localhost dbname=weft user=weft password=weft sslmode=disable"
		log.Println("WARNING: using default DB DSN — set --db-dsn or WEFT_DB_DSN in production")
	}

	store, err := weftapi.NewStore(dsn)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer store.Close()

	srv := weftapi.NewServer(store, jwtSecret, *coordAddr, *baseURL)
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
	srv.StripeWebhookSecret = os.Getenv("STRIPE_WEBHOOK_SECRET")

	log.Printf("weft-api listening on :%s (coordinator: %s)", *port, *coordAddr)
	if err := http.ListenAndServe(":"+*port, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
