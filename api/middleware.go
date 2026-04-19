package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/kevlarcon/weft/store"
)

type contextKey string

const claimsKey contextKey = "claims"

// AuthMiddleware validates JWT tokens from the Authorization header.
func AuthMiddleware(secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		claims, err := ValidateJWT(token, secret)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), claimsKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireEmailVerified gates state-changing endpoints on a verified email.
// Must be applied BEFORE the JWT middleware on the request path — it reads
// claims out of the context that AuthMiddleware inserts.
//
// The JWT's email_verified claim is advisory only; a user who was issued an
// unverified token and later clicked their link still has a "false" claim
// in their JWT until it's refreshed, so this middleware re-reads the DB. A
// 403 with a structured error code lets the frontend surface the
// verify-your-email banner rather than a generic auth failure.
func RequireEmailVerified(st *store.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, _ := r.Context().Value(claimsKey).(*JWTClaims)
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

// GetClaims extracts JWT claims from the request context.
func GetClaims(r *http.Request) *JWTClaims {
	claims, _ := r.Context().Value(claimsKey).(*JWTClaims)
	return claims
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
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
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
