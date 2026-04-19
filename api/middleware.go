package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/kevlarcon/weft/auth"
	"github.com/kevlarcon/weft/store"
)

type contextKey string

// AuthMiddleware validates JWT tokens from the Authorization header and
// stashes the claims in the request context via auth.WithClaims so
// handlers can pull them through GetClaims (REST) or auth.FromContext
// (any protocol).
func AuthMiddleware(secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Authorization")
		if !strings.HasPrefix(hdr, "Bearer ") {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(hdr, "Bearer ")
		claims, err := auth.Validate(token, secret)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithClaims(r.Context(), claims)))
	})
}

// RequireEmailVerified gates state-changing endpoints on a verified email.
// Reads claims out of the ctx that AuthMiddleware inserts, then re-reads
// the DB so a user who verified after their JWT was issued doesn't need
// to re-login to unlock.
func RequireEmailVerified(st *store.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims := auth.FromContext(r.Context())
		if claims == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		user, err := st.GetUserByID(claims.UserID)
		if err != nil || user == nil {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		if user.EmailVerifiedAt == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = writeJSON(w, map[string]any{
				"code":    "email_verification_required",
				"message": "Verify your email to unlock this feature.",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeJSON is a tiny helper to keep the middleware body readable.
func writeJSON(w http.ResponseWriter, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// GetClaims extracts JWT claims from the REST request context.
func GetClaims(r *http.Request) *auth.Claims {
	return auth.FromContext(r.Context())
}

// agentAuthKey holds the validated agent auth result in the request context.
const agentAuthKey contextKey = "agentAuth"

// AgentAuthResult is the identity derived from a validated agent token.
type AgentAuthResult struct {
	CustomerID   uint
	CustomerName string
	Namespace    string
}

// AgentTokenMiddleware validates weftd agent tokens (wft_...) from the
// Authorization header, resolving (customer, namespace) from the token.
func AgentTokenMiddleware(s *store.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Authorization")
		if !strings.HasPrefix(hdr, "Bearer ") {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(hdr, "Bearer ")
		storeResult, err := s.ValidateAgentToken(token)
		if err != nil || storeResult == nil {
			http.Error(w, "invalid or revoked token", http.StatusUnauthorized)
			return
		}
		result := &AgentAuthResult{
			CustomerID:   storeResult.Customer.ID,
			CustomerName: storeResult.Customer.Name,
			Namespace:    storeResult.Namespace,
		}
		ctx := context.WithValue(r.Context(), agentAuthKey, result)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetAgentAuth extracts agent auth from the request context.
func GetAgentAuth(r *http.Request) *AgentAuthResult {
	result, _ := r.Context().Value(agentAuthKey).(*AgentAuthResult)
	return result
}

// CORSMiddleware adds CORS headers for the SPA. Allows the configured
// origin plus localhost for development.
func CORSMiddleware(origin string, next http.Handler) http.Handler {
	allowed := map[string]bool{
		origin: true,
	}
	// Allow common dev origins.
	for _, port := range []string{"5173", "5174", "3000"} {
		allowed["http://localhost:"+port] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqOrigin := r.Header.Get("Origin")
		if allowed[reqOrigin] {
			w.Header().Set("Access-Control-Allow-Origin", reqOrigin)
		} else {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
