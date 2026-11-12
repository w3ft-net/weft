package auth

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted"
)

// Session is the resolved identity for an authenticated request. It is
// distinct from Claims: Claims are the (slim) JWT payload — proof of
// who is calling — and Session is the full picture, materialized from
// the DB once per request. Handlers read Session; the JWT itself is
// only the user ID.
//
// ActiveOrgID/Name describe the org the user is currently scoped
// to. A user can be a member of many orgs; the active one is chosen
// per request via the X-Active-Org header (or a CLI flag), defaulting
// to the user's last-active and finally to their oldest membership.
//
// A session says who the caller is, not what they may do: writes are decided
// by the Authorizer, from the member's OrgPermission row, on each request.
type Session struct {
	UserID             string
	Email              string
	ActiveOrgID        store.OrgID
	ActiveCustomerName string
	// LastActiveOrgID is what users.last_active_org_id held when the session
	// was built, so callers can tell whether pinning ActiveOrgID is a change.
	LastActiveOrgID *store.OrgID
	Tier            store.Tier
	EmailVerified   bool
	IsOwner         bool
	IsSuperadmin    bool
}

// Actor is the session as the party the Scoper and Authorizer judge. Every
// reader of the session's org goes through it, so the partition has one source.
func (s *Session) Actor() hosted.Actor {
	return hosted.Actor{Org: s.ActiveOrgID, User: s.UserID}
}

// Org is the partition the session's store calls are scoped by: the actor's.
func (s *Session) Org() store.OrgID { return s.ActiveOrgID }

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

// ErrNoMembership is returned when a user has no memberships rows —
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
//  2. user.last_active_org_id (if still a valid membership).
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

	// The org switcher needs each org's name, which is what ListUserOrgs
	// returns directly — the membership rows themselves carry only org_id.
	userOrgs, err := st.ListUserOrgs(user.ID)
	if err != nil {
		return nil, fmt.Errorf("load memberships: %w", err)
	}
	if len(userOrgs) == 0 {
		// A superadmin is a platform operator who may legitimately belong
		// to no customer org. Hand back an org-less session (no active
		// customer, tier "none") rather than ErrNoMembership — otherwise
		// AuthMiddleware 403s every authed request including /api/auth/me,
		// and the SPA wipes the token on the first hard navigation, booting
		// the operator to the login screen. Org-scoped endpoints they can't
		// use without an org resolve to org_id=0 (empty results), and
		// they reach the admin surface via RequireSuperadmin regardless.
		if user.IsSuperadmin {
			return &Session{
				UserID:        user.ID,
				Email:         user.Email,
				Tier:          store.Tier("none"),
				EmailVerified: user.EmailVerifiedAt != nil,
				IsSuperadmin:  true,
			}, nil
		}
		return nil, ErrNoMembership
	}

	active, err := selectActiveOrg(userOrgs, user.LastActiveOrgID, orgSelector)
	if err != nil {
		return nil, err
	}

	// saaskit's org ids are opaque strings; this is where weft's become typed.
	activeID, err := store.ParseOrgID(active.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve active org: %w", err)
	}

	tier := st.TierForOrg(activeID)
	isOwner, err := st.IsOwner(activeID, user.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve org role: %w", err)
	}

	return &Session{
		UserID:             user.ID,
		Email:              user.Email,
		ActiveOrgID:        activeID,
		ActiveCustomerName: active.Name,
		LastActiveOrgID:    user.LastActiveOrgID,
		Tier:               tier,
		EmailVerified:      user.EmailVerifiedAt != nil,
		IsOwner:            isOwner,
		IsSuperadmin:       user.IsSuperadmin,
	}, nil
}

// selectActiveOrg picks which org to scope the session to. The slice is
// assumed sorted by membership created_at ASC (oldest first).
func selectActiveOrg(userOrgs []orgs.OrgMembership, lastActive *store.OrgID, selector string) (*orgs.OrgMembership, error) {
	if selector != "" {
		for i := range userOrgs {
			if userOrgs[i].Name == selector {
				return &userOrgs[i], nil
			}
		}
		return nil, ErrOrgNotMember
	}
	if lastActive != nil {
		for i := range userOrgs {
			if userOrgs[i].ID == lastActive.String() {
				return &userOrgs[i], nil
			}
		}
		// Stale last-active (user removed from that org) — fall through.
	}
	return &userOrgs[0], nil
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
