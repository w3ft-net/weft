package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
)

// Server is the weft API server.
type Server struct {
	store     *Store
	jwtSecret string
	coordAddr string // coordinator gRPC address
	baseURL   string // e.g., https://w3ft.net
	email     *EmailConfig // nil = dev mode (log links)

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

	mux.Handle("/api/auth/me", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/billing/checkout", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/billing/usage", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/queries", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/queries/", AuthMiddleware(s.jwtSecret, auth))

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

	// TODO: create Stripe checkout session with priceID
	_ = priceID
	json.NewEncoder(w).Encode(map[string]string{
		"url": fmt.Sprintf("https://checkout.stripe.com/placeholder?tier=%s", req.Tier),
	})
}

func (s *Server) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	// TODO: verify Stripe signature, process events
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// TODO: query usage from metering
	json.NewEncoder(w).Encode(map[string]any{
		"bytes_ingested": 0,
		"period_start":  "",
		"tier":          claims.Tier,
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
