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
	dbPath := flag.String("db", "/opt/app/data/weft-api.db", "SQLite database path")
	coordAddr := flag.String("coordinator", "localhost:9020", "coordinator gRPC address")
	baseURL := flag.String("base-url", "https://w3ft.net", "base URL for magic links")
	flag.Parse()

	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		jwtSecret = "dev-secret-change-in-production"
		log.Println("WARNING: using default JWT secret — set JWT_SECRET in production")
	}

	store, err := weftapi.NewStore(*dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer store.Close()

	srv := weftapi.NewServer(store, jwtSecret, *coordAddr, *baseURL)

	// Configure Stripe prices from env.
	if p := os.Getenv("STRIPE_PRICE_STARTER"); p != "" {
		srv.StripePrices["starter"] = p
	}
	if p := os.Getenv("STRIPE_PRICE_GROWTH"); p != "" {
		srv.StripePrices["growth"] = p
	}
	if p := os.Getenv("STRIPE_PRICE_SCALE"); p != "" {
		srv.StripePrices["scale"] = p
	}
	srv.StripeSecretKey = os.Getenv("STRIPE_SECRET_KEY")
	srv.StripeWebhookSecret = os.Getenv("STRIPE_WEBHOOK_SECRET")

	log.Printf("weft-api listening on :%s (coordinator: %s)", *port, *coordAddr)
	if err := http.ListenAndServe(":"+*port, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
