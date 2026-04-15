package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/checkout/session"
	"github.com/stripe/stripe-go/v82/webhook"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/store"
)

// Server is the weft API server.
type Server struct {
	store     *store.Store
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

	// Slack OAuth config
	SlackClientID     string
	SlackClientSecret string
}

// apiBaseURL returns the API's own external URL (e.g., https://api.w3ft.net).
// Derived from baseURL by inserting "api." prefix.
func (s *Server) apiBaseURL() string {
	// https://w3ft.net → https://api.w3ft.net
	return strings.Replace(s.baseURL, "://", "://api.", 1)
}

// NewServer creates an API server.
func NewServer(s *store.Store, jwtSecret, coordAddr, baseURL string) *Server {
	return &Server{
		store:        s,
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
	mux.HandleFunc("POST /api/auth/device", s.handleDeviceInit)
	mux.HandleFunc("POST /api/auth/device/poll", s.handleDevicePoll)
	mux.HandleFunc("POST /api/billing/webhook", s.handleStripeWebhook)

	// Authenticated routes.
	auth := http.NewServeMux()
	auth.HandleFunc("GET /api/auth/me", s.handleMe)
	auth.HandleFunc("POST /api/auth/device/approve", s.handleDeviceApprove)
	auth.HandleFunc("POST /api/billing/checkout", s.handleCheckout)
	auth.HandleFunc("GET /api/billing/usage", s.handleUsage)
	auth.HandleFunc("GET /api/queries", s.handleListQueries)
	auth.HandleFunc("POST /api/queries", s.handleCreateQuery)
	auth.HandleFunc("DELETE /api/queries/{id}", s.handleDeleteQuery)
	auth.HandleFunc("GET /api/tokens", s.handleListTokens)
	auth.HandleFunc("POST /api/tokens", s.handleCreateToken)
	auth.HandleFunc("DELETE /api/tokens/{token}", s.handleRevokeToken)
	auth.HandleFunc("GET /api/apps", s.handleListApps)
	auth.HandleFunc("POST /api/shell/exec", s.handleShellExec)
	auth.HandleFunc("GET /api/functions", s.handleListFunctions)
	auth.HandleFunc("GET /api/functions/{name}", s.handleGetFunction)
	auth.HandleFunc("PUT /api/functions/{name}", s.handleSaveFunction)
	auth.HandleFunc("DELETE /api/functions/{name}", s.handleDeleteFunction)
	auth.HandleFunc("GET /api/watches", s.handleListWatches)
	auth.HandleFunc("GET /api/watches/{name}", s.handleGetWatch)
	auth.HandleFunc("PUT /api/watches/{name}", s.handleSaveWatch)
	auth.HandleFunc("DELETE /api/watches/{name}", s.handleDeleteWatch)
	auth.HandleFunc("GET /api/watches/{name}/results", s.handleWatchResults)
	auth.HandleFunc("GET /api/integrations", s.handleListIntegrations)
	auth.HandleFunc("POST /api/integrations/slack/connect", s.handleSlackConnect)
	auth.HandleFunc("DELETE /api/integrations/slack", s.handleSlackDisconnect)
	auth.HandleFunc("PUT /api/integrations/pagerduty", s.handlePagerDutySave)
	auth.HandleFunc("DELETE /api/integrations/pagerduty", s.handlePagerDutyDisconnect)
	auth.HandleFunc("PUT /api/integrations/routes", s.handleSaveRoutes)
	auth.HandleFunc("GET /api/namespaces", s.handleListNamespaces)
	auth.HandleFunc("POST /api/namespaces", s.handleCreateNamespace)
	auth.HandleFunc("PUT /api/namespaces/{name}/permissions/{userId}", s.handleSetNamespacePermission)
	auth.HandleFunc("GET /api/team", s.handleListTeam)
	auth.HandleFunc("PUT /api/team/name", s.handleUpdateTeamName)
	auth.HandleFunc("POST /api/team/invite", s.handleInviteTeamMember)
	auth.HandleFunc("DELETE /api/team/{id}", s.handleRemoveTeamMember)

	// Public routes (no auth).
	mux.HandleFunc("POST /api/team/accept", s.handleAcceptInvite)
	mux.HandleFunc("GET /api/integrations/slack/callback", s.handleSlackCallback)

	mux.Handle("/api/auth/me", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/auth/device/approve", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/billing/checkout", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/billing/usage", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/queries", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/queries/", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/tokens", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/tokens/", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/apps", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/shell/exec", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/functions", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/functions/", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/watches", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/watches/", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/integrations", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/integrations/", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/namespaces", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/namespaces/", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/team", AuthMiddleware(s.jwtSecret, auth))
	mux.Handle("/api/team/", AuthMiddleware(s.jwtSecret, auth))

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
		Token   string `json:"token"`
		OrgName string `json:"org_name"` // required for new users
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}

	email, err := s.store.ValidateMagicLink(req.Token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	var user *store.User
	tier := "trial"

	// Check for pending team invite first.
	if invite, err := s.store.GetPendingTeamMemberByEmail(email); err == nil {
		// Invited user — join that customer.
		cust, _ := s.store.GetCustomerByName(invite.CustomerName)
		if cust == nil {
			http.Error(w, "customer not found", http.StatusInternalServerError)
			return
		}
		user, err = s.store.GetUserByEmailAndCustomer(email, cust.ID)
		if err != nil {
			user, err = s.store.CreateUser(email, cust.ID)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		s.store.AcceptTeamMember(invite.ID, user.ID)

		if sub, _ := s.store.ActiveSubscription(invite.CustomerName); sub != nil {
			tier = sub.Tier
		}
	} else {
		// No invite — find existing user or create new user + customer.
		user, _ = s.store.GetUserByEmail(email)
		if user == nil {
			// New user — require org name.
			if req.OrgName == "" {
				// Tell the frontend to prompt for org name.
				json.NewEncoder(w).Encode(map[string]any{
					"needs_org": true,
					"email":     email,
				})
				return
			}

			customerName := strings.ToLower(strings.TrimSpace(req.OrgName))
			cust, _ := s.store.FindOrCreateCustomer(customerName)
			user, err = s.store.CreateUser(email, cust.ID)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}

			// Create trial subscription.
			trialEnd := time.Now().Add(14 * 24 * time.Hour)
			s.store.CreateSubscription(&store.Subscription{
				ID:         uuid.New().String(),
				CustomerID: customerName,
				Tier:       "trial",
				Status:     "active",
				StartsAt:   time.Now(),
				ExpiresAt:  &trialEnd,
			})

			// Create owner team membership.
			s.store.CreateTeamMember(&store.TeamMember{
				ID:           uuid.New().String(),
				CustomerName: customerName,
				Email:        email,
				UserID:       &user.ID,
				Role:         "owner",
				Status:       "active",
				InvitedAt:    time.Now(),
				CreatedAt:    time.Now(),
				UpdatedAt:    time.Now(),
			})
		} else {
			if sub, _ := s.store.ActiveSubscription(user.Customer.Name); sub != nil {
				tier = sub.Tier
			}
		}
	}

	// Consume the magic link now that we have a user.
	s.store.ConsumeMagicLink(req.Token)

	jwt, err := GenerateJWT(user, tier, s.jwtSecret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]any{
		"token": jwt,
		"user": map[string]any{
			"id":    user.ID,
			"email": user.Email,
			"tier":  tier,
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

	tier := "none"
	if s.store != nil {
		if sub, _ := s.store.ActiveSubscription(user.CustomerName); sub != nil {
			tier = sub.Tier
		}
	}

	json.NewEncoder(w).Encode(map[string]any{
		"id":       user.ID,
		"email":    user.Email,
		"name":     user.Name,
		"tier":     tier,
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
		SuccessURL:        stripe.String(s.baseURL + "/account?checkout=success"),
		CancelURL:         stripe.String(s.baseURL + "/account?checkout=cancel"),
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
	params.AddMetadata("customer", claims.CustomerName)

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
		customerName := sess.Metadata["customer"]
		if userID != "" && tier != "" {
			// Store Stripe customer ID on the user.
			if sess.Customer != nil {
				_ = s.store.UpdateUserStripeCustomer(userID, sess.Customer.ID)
			}
			// Create a new active subscription.
			if s.store != nil && customerName != "" {
				subID := ""
				if sess.Subscription != nil {
					subID = sess.Subscription.ID
				}
				_ = s.store.CreateSubscription(&store.Subscription{
					ID:                   uuid.New().String(),
					CustomerID:           customerName,
					Tier:                 tier,
					Status:               "active",
					StartsAt:             time.Now(),
					StripeSubscriptionID: subID,
				})
				log.Printf("subscription activated: customer=%s tier=%s", customerName, tier)
			}
		}

	case "customer.subscription.deleted":
		var sub stripe.Subscription
		if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
			log.Printf("Stripe webhook unmarshal error: %v", err)
			break
		}
		if s.store != nil {
			if err := s.store.CancelSubscription(sub.ID); err != nil {
				log.Printf("failed to cancel subscription %s: %v", sub.ID, err)
			} else {
				log.Printf("subscription cancelled: %s", sub.ID)
			}
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
	if s.store != nil {
		if sub, _ := s.store.ActiveSubscription(claims.CustomerName); sub == nil {
			http.Error(w, "no active subscription", http.StatusPaymentRequired)
			return
		}
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

	// Get the actual tier from the DB subscription, not the JWT (which may be stale).
	tier := claims.Tier
	var tierLimitBytes int64 = 10 * 1024 * 1024 * 1024 // default 10 GB
	if s.store != nil {
		if sub, _ := s.store.ActiveSubscription(claims.CustomerName); sub != nil {
			tier = sub.Tier
			switch sub.Tier {
			case "swatch":
				tierLimitBytes = 10 * 1024 * 1024 * 1024    // 10 GB
			case "yard":
				tierLimitBytes = 100 * 1024 * 1024 * 1024   // 100 GB
			case "bolt":
				tierLimitBytes = 1024 * 1024 * 1024 * 1024  // 1 TB
			case "trial":
				tierLimitBytes = 1 * 1024 * 1024 * 1024     // 1 GB
			}
		}
	}

	result := map[string]any{
		"bytes_ingested":   resp.BytesIngested,
		"tier_limit_bytes": tierLimitBytes,
		"period_start":     resp.PeriodStart,
		"period_end":       resp.PeriodEnd,
		"tier":             tier,
	}

	// Add metrics forecast if available.
	if s.store != nil {
		if cust, err := s.store.GetCustomerByName(claims.CustomerName); err == nil {
			if forecast, err := s.store.ForecastMetricsUsage(cust.ID); err == nil && forecast.AgentsWithMetrics > 0 {
				result["metrics_forecast"] = map[string]any{
					"agents_with_metrics":     forecast.AgentsWithMetrics,
					"projected_bytes_month":   forecast.ProjectedBytesMonth,
				}
			}
		}
	}

	json.NewEncoder(w).Encode(result)
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
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	tokens, err := s.store.ListAgentTokens(custID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	type tokenResult struct {
		Token     string `json:"token"`
		Label     string `json:"label"`
		Namespace string `json:"namespace"`
		Status    string `json:"status"`
		CreatedAt string `json:"created_at"`
	}
	out := make([]tokenResult, 0, len(tokens))
	for _, t := range tokens {
		status := "active"
		if t.Revoked {
			status = "revoked"
		}
		out = append(out, tokenResult{
			Token:     t.Token,
			Label:     t.Label,
			Namespace: t.Namespace.Name,
			Status:    status,
			CreatedAt: t.CreatedAt.Format(time.RFC3339),
		})
	}
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var req struct {
		Label     string `json:"label"`
		Namespace string `json:"namespace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Label == "" {
		http.Error(w, "label required", http.StatusBadRequest)
		return
	}
	if req.Namespace == "" {
		http.Error(w, "namespace required", http.StatusBadRequest)
		return
	}

	// Validate that the namespace exists.
	nsObj, err := s.store.FindOrCreateNamespace(custID, req.Namespace)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Check permission: owner or can_create_tokens on namespace.
	if !s.isOwner(claims.CustomerName, claims.Email) {
		perm, err := s.store.GetNamespacePermission(claims.UserID, nsObj.ID)
		if err != nil || !perm.CanCreateTokens {
			http.Error(w, "forbidden: no token permission on this namespace", http.StatusForbidden)
			return
		}
	}

	tok, err := s.store.CreateAgentToken(custID, nsObj.ID, req.Label)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"token": tok.Token})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}

	if err := s.store.RevokeAgentToken(token); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- Apps ---

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	if s.coordClient == nil {
		http.Error(w, "coordinator unavailable", http.StatusServiceUnavailable)
		return
	}
	claims := GetClaims(r)
	customer := ""
	if claims != nil {
		customer = claims.CustomerName
	}
	resp, err := s.coordClient.ListApps(context.Background(), &pb.ListAppsRequest{Customer: customer})
	if err != nil {
		http.Error(w, "coordinator error", http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(resp.Apps)
}

// --- Shell functions (/bin/) ---

func (s *Server) resolveCustomerID(customerName string) (uint, error) {
	cust, err := s.store.FindOrCreateCustomer(customerName)
	if err != nil {
		return 0, err
	}
	return cust.ID, nil
}

func (s *Server) handleListFunctions(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	fns, err := s.store.ListShellFunctions(custID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"functions": fns,
	})
}

func (s *Server) handleGetFunction(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	fn, err := s.store.GetShellFunction(custID, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(fn)
}

func (s *Server) handleSaveFunction(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	var req struct {
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Source == "" {
		http.Error(w, "source required", http.StatusBadRequest)
		return
	}
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.store.SaveShellFunction(custID, name, req.Source); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDeleteFunction(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.store.DeleteShellFunction(custID, name); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Cron watches (/cron.d/) ---

func (s *Server) handleListWatches(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	watches, err := s.store.ListCronWatches(custID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tier := "trial"
	if sub, _ := s.store.ActiveSubscription(claims.CustomerName); sub != nil {
		tier = sub.Tier
	}
	count, _ := s.store.CountCronWatches(custID)
	json.NewEncoder(w).Encode(map[string]any{
		"watches":      watches,
		"count":        count,
		"limit":        store.WatchLimit(tier),
		"min_interval": store.MinWatchInterval(tier).String(),
	})
}

func (s *Server) handleGetWatch(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w_, err := s.store.GetCronWatch(custID, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(w_)
}

func (s *Server) handleSaveWatch(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	var req struct {
		Schedule   string `json:"schedule"`
		Pipeline   string `json:"pipeline"`
		NotifyDest string `json:"notify_dest"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Schedule == "" || req.Pipeline == "" {
		http.Error(w, "schedule and pipeline required", http.StatusBadRequest)
		return
	}
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Enforce tier limits on watches.
	tier := claims.Tier
	maxWatches := store.WatchLimit(tier)
	if maxWatches == 0 {
		http.Error(w, "watches are not available on the trial plan", http.StatusForbidden)
		return
	}

	// Check watch count (only for new watches, not updates).
	if _, err := s.store.GetCronWatch(custID, name); err != nil {
		// New watch — check count limit.
		count, err := s.store.CountCronWatches(custID)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if count >= maxWatches {
			http.Error(w, fmt.Sprintf("watch limit reached (%d watches on %s plan)", maxWatches, tier), http.StatusForbidden)
			return
		}
	}

	// Check minimum interval.
	minInterval := store.MinWatchInterval(tier)
	interval := store.ParseScheduleInterval(req.Schedule)
	if interval < minInterval {
		http.Error(w, fmt.Sprintf("minimum schedule interval for %s plan is %s", tier, minInterval), http.StatusForbidden)
		return
	}

	if err := s.store.SaveCronWatch(custID, name, req.Schedule, req.Pipeline, req.NotifyDest); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDeleteWatch(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.store.DeleteCronWatch(custID, name); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWatchResults(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	watch, err := s.store.GetCronWatch(custID, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	results, err := s.store.ListCronResults(watch.ID, 50)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(results)
}

// --- Team management ---

func (s *Server) handleListTeam(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	if claims.CustomerName == "" {
		json.NewEncoder(w).Encode(map[string]any{"members": []any{}, "seats_used": 0, "seats_limit": 1})
		return
	}
	members, err := s.store.ListTeamMembers(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	count, _ := s.store.CountTeamMembers(claims.CustomerName)
	tier := "trial"
	if sub, _ := s.store.ActiveSubscription(claims.CustomerName); sub != nil {
		tier = sub.Tier
	}
	orgName := claims.CustomerName
	if cust, err := s.store.GetCustomerByName(claims.CustomerName); err == nil && cust.DisplayName != "" {
		orgName = cust.DisplayName
	}
	json.NewEncoder(w).Encode(map[string]any{
		"members":     members,
		"seats_used":  count,
		"seats_limit": store.SeatLimit(tier),
		"org_name":    orgName,
	})
}

func (s *Server) handleInviteTeamMember(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	if claims.CustomerName == "" {
		http.Error(w, "no team configured", http.StatusBadRequest)
		return
	}

	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Email == "" {
		http.Error(w, "email required", http.StatusBadRequest)
		return
	}

	// Check seat limit.
	tier := "trial"
	if sub, _ := s.store.ActiveSubscription(claims.CustomerName); sub != nil {
		tier = sub.Tier
	}
	count, _ := s.store.CountTeamMembers(claims.CustomerName)
	limit := store.SeatLimit(tier)
	if count >= limit {
		http.Error(w, fmt.Sprintf("seat limit reached (%d/%d)", count, limit), http.StatusForbidden)
		return
	}

	// Check for duplicate.
	if existing, _ := s.store.GetTeamMemberByEmail(claims.CustomerName, req.Email); existing != nil {
		http.Error(w, "already a team member", http.StatusConflict)
		return
	}

	// Create pending member.
	member := &store.TeamMember{
		ID:           uuid.New().String(),
		CustomerName: claims.CustomerName,
		Email:        req.Email,
		Role:         "member",
		Status:       "pending",
		InvitedBy:    claims.UserID,
		InvitedAt:    time.Now(),
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	if err := s.store.CreateTeamMember(member); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Send invite email.
	token, err := s.store.CreateMagicLink(req.Email)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	verifyURL := fmt.Sprintf("%s/verify?token=%s&invite=1", s.baseURL, token)
	// Use display name for the email if available.
	orgName := claims.CustomerName
	if cust, err := s.store.GetCustomerByName(claims.CustomerName); err == nil && cust.DisplayName != "" {
		orgName = cust.DisplayName
	}
	SendInviteEmail(s.email, req.Email, claims.Email, orgName, verifyURL)

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(member)
}

func (s *Server) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}

	// Validate magic link.
	email, err := s.store.ValidateMagicLink(req.Token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	// Find pending invite.
	member, err := s.store.GetPendingTeamMemberByEmail(email)
	if err != nil {
		http.Error(w, "no pending invite found", http.StatusNotFound)
		return
	}

	// Find or create user under the invite's customer.
	cust, _ := s.store.GetCustomerByName(member.CustomerName)
	if cust == nil {
		http.Error(w, "customer not found", http.StatusInternalServerError)
		return
	}
	user, _ := s.store.GetUserByEmailAndCustomer(email, cust.ID)
	if user == nil {
		user, _ = s.store.CreateUser(email, cust.ID)
	}
	s.store.AcceptTeamMember(member.ID, user.ID)

	tier := "trial"
	if sub, _ := s.store.ActiveSubscription(member.CustomerName); sub != nil {
		tier = sub.Tier
	}

	jwt, err := GenerateJWT(user, tier, s.jwtSecret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]any{
		"token": jwt,
		"user": map[string]any{
			"id":    user.ID,
			"email": user.Email,
			"tier":  tier,
		},
	})
}

func (s *Server) handleRemoveTeamMember(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}

	// Verify caller is owner.
	callerMember, err := s.store.GetTeamMemberByEmail(claims.CustomerName, claims.Email)
	if err != nil || callerMember.Role != "owner" {
		http.Error(w, "only the owner can remove members", http.StatusForbidden)
		return
	}

	// Can't remove self.
	target, err := s.store.GetTeamMemberByID(id)
	if err != nil {
		http.Error(w, "member not found", http.StatusNotFound)
		return
	}
	if target.Email == claims.Email {
		http.Error(w, "cannot remove yourself", http.StatusBadRequest)
		return
	}

	// Remove.
	s.store.RemoveTeamMember(id)

	// Note: we don't clear the user's CustomerID — the user record
	// stays associated with the customer for audit purposes. The
	// TeamMember status=removed controls access.

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUpdateTeamName(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)

	// Only the owner can rename.
	if callerMember, err := s.store.GetTeamMemberByEmail(claims.CustomerName, claims.Email); err != nil || callerMember.Role != "owner" {
		http.Error(w, "only the owner can rename the team", http.StatusForbidden)
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	if err := s.store.UpdateCustomerDisplayName(claims.CustomerName, req.Name); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"name": req.Name})
}

// ── Device auth flow (CLI login) ─────────────────────────────────────

// handleDeviceInit creates a new device code for CLI authorization.
// POST /api/auth/device → {"device_code": "...", "user_code": "XXXX-XX", "expires_in": 600}
func (s *Server) handleDeviceInit(w http.ResponseWriter, r *http.Request) {
	dc, err := s.store.CreateDeviceCode()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"device_code": dc.DeviceCode,
		"user_code":   dc.UserCode,
		"expires_in":  600,
	})
}

// handleDevicePoll checks if a device code has been approved.
// POST /api/auth/device/poll {"device_code": "..."}
// Returns {"status": "pending"} or {"status": "approved", "token": "jwt..."}
func (s *Server) handleDevicePoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeviceCode == "" {
		http.Error(w, "device_code required", http.StatusBadRequest)
		return
	}

	userID, err := s.store.PollDeviceCode(req.DeviceCode)
	if err != nil {
		http.Error(w, err.Error(), http.StatusGone)
		return
	}
	if userID == "" {
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
		return
	}

	user, err := s.store.GetUserByID(userID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	tier := "trial"
	if sub, _ := s.store.ActiveSubscription(user.CustomerName); sub != nil {
		tier = sub.Tier
	}

	jwt, err := GenerateJWT(user, tier, s.jwtSecret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]any{
		"status":   "approved",
		"token":    jwt,
		"email":    user.Email,
		"customer": user.CustomerName,
	})
}

// handleDeviceApprove approves a device code (requires auth — user clicks in browser).
// POST /api/auth/device/approve {"user_code": "XXXX-XX"}
func (s *Server) handleDeviceApprove(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		UserCode string `json:"user_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserCode == "" {
		http.Error(w, "user_code required", http.StatusBadRequest)
		return
	}

	if err := s.store.ApproveDeviceCode(req.UserCode, claims.UserID); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "approved"})
}

// ── Namespace management ──────────────────────────────────────

func (s *Server) handleListNamespaces(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	nss, err := s.store.ListNamespaces(custID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	type nsResult struct {
		Name string `json:"name"`
		ID   uint   `json:"id"`
	}
	out := make([]nsResult, len(nss))
	for i, ns := range nss {
		out[i] = nsResult{Name: ns.Name, ID: ns.ID}
	}
	json.NewEncoder(w).Encode(map[string]any{"namespaces": out})
}

func (s *Server) handleCreateNamespace(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	custID, err := s.resolveCustomerID(claims.CustomerName)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Check permission: owner or CanCreateNamespace.
	user, err := s.store.GetUserByID(claims.UserID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	isOwner := s.isOwner(claims.CustomerName, claims.Email)
	if !isOwner && !user.CanCreateNamespace {
		http.Error(w, "forbidden: cannot create namespaces", http.StatusForbidden)
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	ns, err := s.store.FindOrCreateNamespace(custID, req.Name)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Grant creator full access.
	s.store.GrantFullNamespaceAccess(claims.UserID, ns.ID)

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"name": ns.Name, "id": ns.ID})
}

func (s *Server) handleSetNamespacePermission(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	if !s.isOwner(claims.CustomerName, claims.Email) {
		http.Error(w, "forbidden: owner only", http.StatusForbidden)
		return
	}

	nsName := r.PathValue("name")
	userID := r.PathValue("userId")
	if nsName == "" || userID == "" {
		http.Error(w, "name and userId required", http.StatusBadRequest)
		return
	}

	custID, _ := s.resolveCustomerID(claims.CustomerName)
	ns, err := s.store.FindOrCreateNamespace(custID, nsName)
	if err != nil {
		http.Error(w, "namespace not found", http.StatusNotFound)
		return
	}

	var req store.NamespacePermission
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.UserID = userID
	req.NamespaceID = ns.ID

	if err := s.store.UpsertNamespacePermission(&req); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// isOwner checks if the email has owner role on the customer team.
func (s *Server) isOwner(customerName, email string) bool {
	member, err := s.store.GetTeamMemberByEmail(customerName, email)
	if err != nil {
		return false
	}
	return member.Role == "owner"
}
