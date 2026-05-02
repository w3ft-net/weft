package auth

import (
	"context"
	"fmt"

	"github.com/kevlarcon/weft/store"
)

// Session is the resolved identity for an authenticated request. It is
// distinct from Claims: Claims are the (slim) JWT payload — proof of
// who is calling — and Session is the full picture, materialized from
// the DB once per request. Handlers read Session; the JWT itself is
// only the user ID.
//
// HomeCustomerID/Name describe the user's *home* tenant — the org they
// were created under, where they own data and pay their bill. They do
// NOT determine which data a query may touch; that decision belongs to
// path-level / namespace-level authorization. Treat HomeCustomer as
// "default landing" and "billing target," not as a security boundary.
type Session struct {
	UserID           string
	Email            string
	HomeCustomerID   uint
	HomeCustomerName string
	Tier             store.Tier
	EmailVerified    bool
	IsHomeOwner      bool
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

// BuildSession materializes a Session for the given user ID by looking
// up the user record and its derived attributes (tier, owner role).
// Called by the auth middleware/interceptor after JWT validation.
func BuildSession(st *store.Store, userID string) (*Session, error) {
	user, err := st.GetUserByID(userID)
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("user not found")
	}

	tier := st.TierForCustomer(user.CustomerID)

	isOwner := false
	if member, err := st.GetTeamMemberByEmail(user.Customer.Name, user.Email); err == nil && member != nil {
		isOwner = member.Role == "owner"
	}

	return &Session{
		UserID:           user.ID,
		Email:            user.Email,
		HomeCustomerID:   user.CustomerID,
		HomeCustomerName: user.Customer.Name,
		Tier:             tier,
		EmailVerified:    user.EmailVerifiedAt != nil,
		IsHomeOwner:      isOwner,
	}, nil
}
