package auth

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/weft/store"
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
// CanManageFunctions/Watches/Etc gate writes to /bin, /cron.d, /etc.
// Owners get all three implicitly; members with no OrgPermission row
// default to all-false (read-only).
type Session struct {
	UserID             string
	Email              string
	ActiveOrgID        string
	ActiveCustomerName string
	Tier               store.Tier
	EmailVerified      bool
	IsOwner            bool
	IsSuperadmin       bool
	CanManageFunctions bool
	CanManageWatches   bool
	CanManageEtc       bool
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

	tier := st.TierForOrg(active.ID)
	isOwner, err := st.IsOwner(active.ID, user.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve org role: %w", err)
	}

	// Owners get implicit write access to all org-scoped dirs; members
	// inherit from their OrgPermission row, defaulting to read-only
	// when no row exists.
	canFunctions, canWatches, canEtc := isOwner, isOwner, isOwner
	if !isOwner {
		perm, err := st.GetOrgPermission(active.ID, user.ID)
		if err == nil && perm != nil {
			canFunctions = perm.CanManageFunctions
			canWatches = perm.CanManageWatches
			canEtc = perm.CanManageEtc
		} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("load org perms: %w", err)
		}
	}

	return &Session{
		UserID:             user.ID,
		Email:              user.Email,
		ActiveOrgID:        active.ID,
		ActiveCustomerName: active.Name,
		Tier:               tier,
		EmailVerified:      user.EmailVerifiedAt != nil,
		IsOwner:            isOwner,
		IsSuperadmin:       user.IsSuperadmin,
		CanManageFunctions: canFunctions,
		CanManageWatches:   canWatches,
		CanManageEtc:       canEtc,
	}, nil
}

// selectActiveOrg picks which org to scope the session to. The slice is
// assumed sorted by membership created_at ASC (oldest first).
func selectActiveOrg(userOrgs []orgs.OrgMembership, lastActive *string, selector string) (*orgs.OrgMembership, error) {
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
			if userOrgs[i].ID == *lastActive {
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
