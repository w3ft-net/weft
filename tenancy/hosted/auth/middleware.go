package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/store"
)

// ActiveOrgHeader is the request header that selects which org (customer) the
// user is acting as for this call. CLI sends it from the --org flag; the web UI
// from a navbar selector. When absent, the middleware falls back to the user's
// last_active_org_id, then to their oldest membership.
const ActiveOrgHeader = "X-Active-Org"

// Middleware validates JWT tokens from the Authorization header, resolves the
// caller's full identity (including which org to scope to) from the DB, and
// stashes both the (slim) claims and the resolved Session on the request
// context. Handlers retrieve the session with SessionFromContext.
//
// When the resolved active org differs from the user's stored
// last_active_org_id, the middleware writes the new value back — that way
// reconnecting later (without an explicit X-Active-Org header) lands the user
// in the org they last operated in.
//
// onSession, if set, is told who the request is, for the access log.
func Middleware(st *store.Store, secret string, onSession func(r *http.Request, userID string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hdr := r.Header.Get("Authorization")
			if !strings.HasPrefix(hdr, "Bearer ") {
				http.Error(w, "missing authorization", http.StatusUnauthorized)
				return
			}
			token := strings.TrimPrefix(hdr, "Bearer ")
			claims, err := Validate(token, secret)
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			session, err := BuildSession(st, claims.UserID, r.Header.Get(ActiveOrgHeader))
			if err != nil {
				switch {
				case errors.Is(err, ErrOrgNotMember):
					http.Error(w, "not a member of the requested org", http.StatusForbidden)
				case errors.Is(err, ErrNoMembership):
					http.Error(w, "no org memberships", http.StatusForbidden)
				default:
					http.Error(w, "unauthorized", http.StatusUnauthorized)
				}
				return
			}
			persistLastActive(r.Context(), st, session)
			if onSession != nil {
				onSession(r, session.UserID)
			}
			ctx := WithClaims(r.Context(), claims)
			ctx = WithSession(ctx, session)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// persistLastActive flips users.last_active_org_id when the resolved active org
// doesn't match what the session loaded. It is synchronous, but the write only
// happens on a change, so the common request pays nothing; failures log but
// never block the request (the next request will retry).
func persistLastActive(ctx context.Context, st *store.Store, session *Session) {
	// An org-less session (a superadmin with no membership) has no active
	// customer to pin. Writing 0 trips the last_active_org_id foreign key, so
	// there's nothing to persist.
	if session.Org() == 0 {
		return
	}
	if session.LastActiveOrgID != nil && *session.LastActiveOrgID == session.Org() {
		return
	}
	if err := st.UpdateUserLastActiveOrg(session.UserID, session.Org()); err != nil {
		weftsdk.Infof(ctx, "update last_active_org_id for %s: %v", session.UserID, err)
	}
}

// RequireEmailVerified gates state-changing endpoints on a verified email.
// Reads the Session that Middleware inserts (which already reflects the current
// DB state), so a user who verified after their JWT was issued passes this gate
// without re-logging in.
func RequireEmailVerified(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := SessionFromContext(r.Context())
		if session == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !session.EmailVerified {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			b, _ := json.Marshal(map[string]any{
				"code":    "email_verification_required",
				"message": "Verify your email to unlock this feature.",
			})
			w.Write(b)
			return
		}
		next.ServeHTTP(w, r)
	})
}
