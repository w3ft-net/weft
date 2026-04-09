package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/webhook"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// Server is the weft API server.
type Server struct {
	store     *Store
	jwtSecret string
	coordAddr string // coordinator gRPC address
	baseURL   string // e.g., https://w3ft.net
	email     *EmailConfig // nil = dev mode (log links)

	// Coordinator gRPC client (connected lazily via Connect).
	coordConn   *grpc.ClientConn
	coordClient pb.WeftCoordinatorClient

	// Stripe config
	StripeSecretKey    string
	StripeWebhookSecret string
	StripePrices       map[string]string // tier → Stripe price ID
}

// NewServer creates an API server.
func NewServer(store *Store, jwtSecret, coordAddr, baseURL string) *Server {
	return &Server{
		store:        store,
		jwtSecret:    jwtSecret,
		coordAddr:    coordAddr,
		baseURL:      baseURL,
		email:        EmailConfigFromEnv(),
		StripePrices: make(map[string]string),
	}
}

// Connect establishes the gRPC connection to the coordinator. It should be
// called once before serving requests. The caller is responsible for calling
// Close when done.
func (s *Server) Connect() error {
	conn, err := grpc.NewClient(s.coordAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("connect to coordinator at %s: %w", s.coordAddr, err)
	}
	s.coordConn = conn
	s.coordClient = pb.NewWeftCoordinatorClient(conn)
	return nil
}

// Close releases the coordinator gRPC connection.
func (s *Server) Close() {
	if s.coordConn != nil {
		s.coordConn.Close()
	}
}

// Handler returns the HTTP handler with all routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public routes (no auth).
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/verify", s.handleVerify)
	mux.HandleFunc("POST /api/billing/webhook", s.handleStripeWebhook)

	// Authenticated routes.
	auth := http.NewServeMux()
	auth.HandleFunc("GET /api/auth/me", s.handleMe)
	auth.HandleFunc("POST /api/billing/checkout", s.handleCheckout)
	auth.HandleFunc("GET /api/billing/usage", s.handleUsage)
	auth.HandleFunc("GET /api/queries", s.handleListQueries)
	auth.HandleFunc("POST /api/queries", s.handleCreateQuery)
	auth.HandleFunc("DELETE /api/queries/{id}", s.handleDeleteQuery)
	auth.HandleFunc("GET /api/tokens", s.handleListTokens)
	auth.HandleFunc("POST /api/tokens", s.handleCreateToken)
	auth.HandleFunc("DELETE /api/tokens/{token}", s.handleRevokeToken)
	auth.HandleFunc("POST /api/shell/exec", s.handleShellExec)

	mux.Handle("/api/auth/me", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/billing/checkout", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/billing/usage", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/queries", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/queries/", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/tokens", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/tokens/", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/shell/exec", AuthMiddleware(s.jwtSecret, auth))

	return CORSMiddleware(s.baseURL, mux)
}

// --- Auth handlers ---

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Email == "" {
		http.Error(w, "email required", http.StatusBadRequest)
		return
	}

	token, err := s.store.CreateMagicLink(req.Email)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	verifyURL := fmt.Sprintf("%s/verify?token=%s", s.baseURL, token)
	if err := SendMagicLinkEmail(s.email, req.Email, verifyURL); err != nil {
		log.Printf("failed to send magic link to %s: %v", req.Email, err)
		http.Error(w, "failed to send email", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}

	user, err := s.store.ValidateMagicLink(req.Token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	jwt, err := GenerateJWT(user, s.jwtSecret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]any{
		"token": jwt,
		"user": map[string]any{
			"id":    user.ID,
			"email": user.Email,
			"tier":  user.Tier,
		},
	})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	user, err := s.store.GetUserByID(claims.UserID)
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(map[string]any{
		"id":       user.ID,
		"email":    user.Email,
		"name":     user.Name,
		"tier":     user.Tier,
		"customer": user.CustomerName,
	})
}

// --- Billing handlers ---

func (s *Server) handleCheckout(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		Tier string `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "tier required", http.StatusBadRequest)
		return
	}

	priceID, ok := s.StripePrices[req.Tier]
	if !ok {
		http.Error(w, "invalid tier", http.StatusBadRequest)
		return
	}

	stripe.Key = s.StripeSecretKey

	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{Price: stripe.String(priceID), Quantity: stripe.Int64(1)},
		},
		SuccessURL:        stripe.String(s.baseURL + "/dashboard?checkout=success"),
		CancelURL:         stripe.String(s.baseURL + "/dashboard?checkout=cancel"),
		ClientReferenceID: stripe.String(claims.UserID),
		CustomerEmail:     stripe.String(claims.Email),
	}
	// If the user already has a Stripe customer, use it.
	user, err := s.store.GetUserByID(claims.UserID)
	if err == nil && user.StripeCustomerID != "" {
		params.Customer = stripe.String(user.StripeCustomerID)
		params.CustomerEmail = nil // can't set both
	}
	params.AddMetadata("tier", req.Tier)
	params.AddMetadata("user_id", claims.UserID)

	sess, err := session.New(params)
	if err != nil {
		log.Printf("Stripe checkout error: %v", err)
		http.Error(w, "failed to create checkout session", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"url": sess.URL})
}

func (s *Server) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 65536))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	event, err := webhook.ConstructEvent(body, r.Header.Get("Stripe-Signature"), s.StripeWebhookSecret)
	if err != nil {
		log.Printf("Stripe webhook signature verification failed: %v", err)
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	switch event.Type {
	case "checkout.session.completed":
		var sess stripe.CheckoutSession
		if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
			log.Printf("Stripe webhook unmarshal error: %v", err)
			break
		}
		userID := sess.Metadata["user_id"]
		tier := sess.Metadata["tier"]
		if userID != "" && tier != "" {
			if err := s.store.UpdateUserStripe(userID, sess.Customer.ID, sess.Subscription.ID, tier); err != nil {
				log.Printf("failed to update user stripe info: %v", err)
			} else {
				log.Printf("subscription activated: user=%s tier=%s", userID, tier)
			}
		}

	case "customer.subscription.deleted":
		var sub stripe.Subscription
		if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
			log.Printf("Stripe webhook unmarshal error: %v", err)
			break
		}
		// Downgrade user back to trial (expired).
		if err := s.store.DowngradeByStripeSubscription(sub.ID); err != nil {
			log.Printf("failed to downgrade subscription %s: %v", sub.ID, err)
		} else {
			log.Printf("subscription cancelled: %s", sub.ID)
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.coordClient == nil {
		http.Error(w, "coordinator unavailable", http.StatusServiceUnavailable)
		return
	}

	resp, err := s.coordClient.GetUsage(context.Background(), &pb.GetUsageRequest{
		Customer: claims.CustomerName,
	})
	if err != nil {
		log.Printf("GetUsage gRPC error: %v", err)
		http.Error(w, "coordinator error", http.StatusBadGateway)
		return
	}

	json.NewEncoder(w).Encode(map[string]any{
		"bytes_ingested": resp.BytesIngested,
		"period_start":   resp.PeriodStart,
		"period_end":     resp.PeriodEnd,
		"tier":           claims.Tier,
	})
}

// --- Saved queries handlers ---

func (s *Server) handleListQueries(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	queries, err := s.store.ListSavedQueries(claims.UserID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(queries)
}

func (s *Server) handleCreateQuery(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	var req struct {
		Name  string `json:"name"`
		Query string `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.Query == "" {
		http.Error(w, "name and query required", http.StatusBadRequest)
		return
	}

	sq, err := s.store.CreateSavedQuery(claims.UserID, req.Name, req.Query)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(sq)
}

func (s *Server) handleDeleteQuery(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteSavedQuery(claims.UserID, id); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Token management handlers ---

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	if s.coordClient == nil {
		http.Error(w, "coordinator unavailable", http.StatusServiceUnavailable)
		return
	}

	resp, err := s.coordClient.ListTokens(context.Background(), &pb.ListTokensRequest{
		Customer: claims.CustomerName,
	})
	if err != nil {
		log.Printf("ListTokens gRPC error: %v", err)
		http.Error(w, "coordinator error", http.StatusBadGateway)
		return
	}

	type tokenResult struct {
		Token     string `json:"token"`
		Label     string `json:"label"`
		App       string `json:"app"`
		Revoked   bool   `json:"revoked"`
		CreatedAt string `json:"created_at"`
	}
	out := make([]tokenResult, 0, len(resp.Tokens))
	for _, t := range resp.Tokens {
		out = append(out, tokenResult{
			Token:     t.Token,
			Label:     t.Label,
			App:       t.App,
			Revoked:   t.Revoked,
			CreatedAt: t.CreatedAt,
		})
	}
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	if s.coordClient == nil {
		http.Error(w, "coordinator unavailable", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Label string `json:"label"`
		App   string `json:"app"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Label == "" {
		http.Error(w, "label required", http.StatusBadRequest)
		return
	}

	resp, err := s.coordClient.CreateToken(context.Background(), &pb.CreateTokenRequest{
		Customer: claims.CustomerName,
		Label:    req.Label,
		App:      req.App,
	})
	if err != nil {
		log.Printf("CreateToken gRPC error: %v", err)
		http.Error(w, "coordinator error", http.StatusBadGateway)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"token": resp.Token})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	if s.coordClient == nil {
		http.Error(w, "coordinator unavailable", http.StatusServiceUnavailable)
		return
	}

	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}

	_, err := s.coordClient.RevokeToken(context.Background(), &pb.RevokeTokenRequest{Token: token})
	if err != nil {
		log.Printf("RevokeToken gRPC error: %v", err)
		http.Error(w, "coordinator error", http.StatusBadGateway)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
