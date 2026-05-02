package auth

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/kevlarcon/weft/store"
)

// Session is the resolved identity for an authenticated request. It is
// distinct from Claims: Claims are the (slim) JWT payload — proof of
// who is calling — and Session is the full picture, materialized from
// the DB once per request. Handlers read Session; the JWT itself is
// only the user ID.
//
// ActiveCustomerID/Name describe the org the user is currently scoped
// to. A user can be a member of many orgs; the active one is chosen
// per request via the X-Active-Org header (or a CLI flag), defaulting
// to the user's last-active and finally to their oldest membership.
type Session struct {
	UserID             string
	Email              string
	ActiveCustomerID   uint
	ActiveCustomerName string
	Tier               store.Tier
	EmailVerified      bool
	IsOwner            bool
}

type sessionContextKey struct{}

var sessionKey = sessionContextKey{}

// WithSession returns a context carrying the resolved session.
func WithSession(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, sessionKey, s)
}

// SessionFromContext pulls the session previously injected by
// WithSession. Returns nil if no session was attached.
func SessionFromContext(ctx context.Context) *Session {
	s, _ := ctx.Value(sessionKey).(*Session)
	return s
}

// ErrNoMembership is returned when a user has no team_members rows —
// they're authenticated but can't act in any org. Surfaces as
// PermissionDenied to the client.
var ErrNoMembership = errors.New("user is not a member of any org")

// ErrOrgNotMember is returned when the requested active org isn't one
// the user belongs to. Surfaces as PermissionDenied.
var ErrOrgNotMember = errors.New("user is not a member of the requested org")

// BuildSession materializes a Session for the given user ID, resolving
// which org to scope to via the precedence:
//
//  1. Explicit selector (e.g. X-Active-Org header) — must match a
//     membership or BuildSession fails.
//  2. user.last_active_customer_id (if still a valid membership).
//  3. The user's oldest active membership.
//
// Returns ErrNoMembership if the user belongs to no org, or
// ErrOrgNotMember if an explicit selector was given but doesn't match.
// Callers (middleware, gRPC interceptor) translate these into
// PermissionDenied.
func BuildSession(st *store.Store, userID string, orgSelector string) (*Session, error) {
	user, err := st.GetUserByID(userID)
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("user not found")
	}

	memberships, err := st.ListUserMemberships(user.ID)
	if err != nil {
		return nil, fmt.Errorf("load memberships: %w", err)
	}
	if len(memberships) == 0 {
		return nil, ErrNoMembership
	}

	active, err := selectActiveMembership(memberships, user.LastActiveCustomerID, orgSelector)
	if err != nil {
		return nil, err
	}

	tier := st.TierForCustomer(active.CustomerID)

	return &Session{
		UserID:             user.ID,
		Email:              user.Email,
		ActiveCustomerID:   active.CustomerID,
		ActiveCustomerName: active.Customer.Name,
		Tier:               tier,
		EmailVerified:      user.EmailVerifiedAt != nil,
		IsOwner:            active.Role == "owner",
	}, nil
}

// selectActiveMembership picks which org to scope the session to. The
// memberships slice is assumed sorted by created_at ASC (oldest first).
func selectActiveMembership(memberships []store.TeamMember, lastActive *uint, selector string) (*store.TeamMember, error) {
	if selector != "" {
		for i := range memberships {
			if memberships[i].Customer.Name == selector {
				return &memberships[i], nil
			}
		}
		return nil, ErrOrgNotMember
	}
	if lastActive != nil {
		for i := range memberships {
			if memberships[i].CustomerID == *lastActive {
				return &memberships[i], nil
			}
		}
		// Stale last-active (user removed from that org) — fall through.
	}
	return &memberships[0], nil
}

// IgnoreNotFound translates gorm.ErrRecordNotFound into nil so callers
// can ergonomically test "is the row absent" without an explicit branch.
// Exported for symmetry with store helpers that already do this inline.
func IgnoreNotFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	return err
}
