package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/graybeardcorp/saaskit/orgs"
	"gorm.io/gorm"
)

// Tier is a plan name. Use the constants below (TierSwatch,
// TierYard, …) rather than bare strings — every tier-keyed function
// in this file switches on these values, and the type keeps typos
// from silently falling into the default/trial branch at call sites.
// The underlying representation is string so GORM can round-trip
// Subscription.Tier without custom serialization.
type Tier string

const (
	TierTrial  Tier = "trial"
	TierSwatch Tier = "swatch"
	TierYard   Tier = "yard"
	TierBolt   Tier = "bolt"
)

// WatchLimit returns the max number of cron watches for a tier.
func WatchLimit(tier Tier) int {
	switch tier {
	case TierSwatch:
		return 20
	case TierYard:
		return 100
	case TierBolt:
		return 500
	default:
		return 1 // trial: no watches
	}
}

// MinWatchInterval returns the minimum allowed schedule interval for a tier.
func MinWatchInterval(tier Tier) time.Duration {
	switch tier {
	case TierSwatch, TierTrial:
		return 5 * time.Minute
	case TierYard, TierBolt:
		return 1 * time.Minute
	default:
		return 0 // trial: watches not allowed
	}
}

// LogRetention returns how long log data is retained for a given plan
// tier. Mirrors the pricing table on the landing page — this is what
// customers are paying for, so treat these values as contractual.
//
// Enforcement is query-side only today: callers filter rows older than
// this cap before returning them to the user. Data on disk can live
// longer (up to the global pyramid boundary in Postgres, indefinitely
// in S3) — see docs/plans/retention-enforcement.md for the path to
// real storage-side enforcement.
func LogRetention(tier Tier) time.Duration {
	switch tier {
	case TierSwatch:
		return 30 * 24 * time.Hour
	case TierYard:
		return 90 * 24 * time.Hour
	case TierBolt:
		return 180 * 24 * time.Hour
	default:
		return 24 * time.Hour // trial
	}
}

// MetricRetention returns how long log-derived metrics (cron_results
// and its rollups) are retained. By construction this equals
// LogRetention — retaining a metric past its source logs would be
// misleading — so callers that care about either can use whichever
// name reads clearer.
func MetricRetention(tier Tier) time.Duration { return LogRetention(tier) }

// VolumeAllowance returns the monthly ingestion quota for a tier, in
// bytes. Uses decimal GB (10⁹) to align with the KB unit used by the
// Stripe metered overage price — so the "% used" bar in the dashboard
// matches what Stripe will bill. The landing page's "10 GB / 100 GB
// / 500 GB" has always been decimal in customer-speak; the internal
// constant was 1024³-based (GiB) before this, disagreeing by 7.4%.
// See docs/plans/stripe.md for the alignment rationale.
func VolumeAllowance(tier Tier) int64 {
	switch tier {
	case TierSwatch:
		return 10 * 1_000_000_000
	case TierYard:
		return 100 * 1_000_000_000
	case TierBolt:
		return 500 * 1_000_000_000
	default:
		return 1 * 1_000_000_000 // trial
	}
}

// TmpStorageAllowance returns the per-customer /tmp storage cap, in
// bytes. Deliberately conservative: /tmp is a scratch pad for filtered
// result sets and quick notes, not long-term storage — the 48h TTL
// (enforced by partman-managed partitions) caps the long-term cost, and
// these limits cap the instantaneous cost.
//
// Ratcheting up later is easy. Ratcheting down after people depend on
// the space is not. Pair with TmpMaxFileBytes, which is a flat cap
// applied on every individual write regardless of tier.
func TmpStorageAllowance(tier Tier) int64 {
	switch tier {
	case TierSwatch:
		return 10 * 1024 * 1024 // 10 MB
	case TierYard:
		return 100 * 1024 * 1024 // 100 MB
	case TierBolt:
		return 1024 * 1024 * 1024 // 1 GB
	default:
		return 500 * 1024 // trial: 500 KB
	}
}

// TmpMaxFileBytes caps the size of any single /tmp file. Applies to
// every tier — raising a customer's total allowance doesn't raise
// their per-file ceiling. Streaming writers enforce this mid-stream,
// so a pipeline filling a tmp file will fail with a clear error
// rather than silently truncating.
const TmpMaxFileBytes int64 = 512 * 1024 // 1/2 MB

// OverageRatePerGB returns the per-GB charge for ingestion over
// VolumeAllowance, in US cents. Matches the pricing page and the
// Stripe graduated metered price. Used only for dashboard projections
// — Stripe is authoritative for actual invoices. Trial tier returns
// 0 (trial subscriptions have no metered item in Stripe; upgrade
// adds it). See docs/plans/stripe.md.
func OverageRatePerGB(tier Tier) int {
	switch tier {
	case TierSwatch:
		return 200 // $2.00
	case TierYard:
		return 100 // $1.00
	case TierBolt:
		return 75 // $0.75
	default:
		return 0
	}
}

// User represents a weft account. A single user can be a member of
// multiple customers (orgs) via memberships rows; the User row itself
// no longer carries a customer pointer. LastActiveOrgID records
// where the user last operated so reconnects land in the same place.
type User struct {
	ID               string `gorm:"primaryKey"`
	Email            string `gorm:"not null;uniqueIndex:idx_users_email"`
	Name             string
	StripeCustomerID string
	IsSuperadmin     bool `gorm:"not null;default:false"`
	// HasSeenWelcome flips true when the user dismisses the first-time
	// welcome modal. The modal re-shows on every login until the flip
	// happens — new users get one chance per session to read the tour.
	HasSeenWelcome bool `gorm:"not null;default:false"`
	// EmailVerifiedAt is the timestamp when the user proved ownership of
	// their email by clicking a magic-link. Nil on a fresh signup until
	// the round-trip completes. State-changing API endpoints block on
	// this being non-nil; the cleanup sweep deletes users where this is
	// still nil more than 24h after CreatedAt.
	EmailVerifiedAt *time.Time
	// LastActiveOrgID is the customer the user was most recently
	// scoped to. The middleware uses it as the default active org when
	// the request omits the X-Active-Org header. Nullable because new
	// users haven't picked one yet.
	LastActiveOrgID *string
	// OnboardingStep tracks where the user is in the Loom-Inc tour
	// state machine. 'welcome' is the default for new signups; 'done'
	// marks a graduate. The middleware reads this to decide whether
	// the welcome modal / tour overlays render; the shell hooks read
	// it to decide whether OnboardingHooks attach.
	OnboardingStep      string `gorm:"not null;default:welcome"`
	OnboardingDismissed bool   `gorm:"not null;default:false"`
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// MagicLink is a one-time-use login token.
type MagicLink struct {
	ID        uint   `gorm:"primaryKey"`
	Email     string `gorm:"not null"`
	Token     string `gorm:"uniqueIndex;not null"`
	ExpiresAt time.Time
	Used      bool `gorm:"not null;default:false"`
}

// TeamMember is an org membership — an alias for the shared
// saaskit/orgs.Membership rather than a weft type that mirrors it, so weft's
// call sites and orgs' return values are literally the same value with no
// conversion layer between them.
//
// Two things changed when weft adopted the shared shape:
//   - UserID is a plain string; the empty string (not a nil pointer) marks an
//     invite nobody has accepted yet. Membership.Pending() reads better than
//     testing it directly.
//   - Role uses orgs' vocabulary: an org's owner is orgs.RoleAdmin ("admin").
//     weft's "owner" meant exactly that, and keeping a second word for it
//     would have made orgs.IsAdmin() disagree with weft about who is in charge.
type TeamMember = orgs.Membership

// RoleOwner is the role granting implicit full access to an org. It is
// orgs.RoleAdmin under weft's name for it; use this constant so the two never
// drift apart.
const RoleOwner = orgs.RoleAdmin

// SeatLimit returns the max number of team members for a tier.
func SeatLimit(tier Tier) int {
	switch tier {
	case TierSwatch:
		return 5
	case TierYard:
		return 10
	case TierBolt:
		return 50
	default:
		return 1 // trial
	}
}

// --- Users ---

// CreateUser inserts a new user with EmailVerifiedAt == nil. Membership
// in any specific customer is established by inserting a TeamMember row
// after creation; the User row itself no longer carries a customer.
func (s *Store) CreateUser(email string) (*User, error) {
	now := time.Now()
	user := User{
		ID:        uuid.New().String(),
		Email:     email,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.db.Create(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

// CreateVerifiedUser is like CreateUser but marks EmailVerifiedAt = now.
// Used by the magic-link signup path where ownership is proven in a
// single round-trip (the user wouldn't receive the email otherwise).
func (s *Store) CreateVerifiedUser(email string) (*User, error) {
	now := time.Now()
	user := User{
		ID:              uuid.New().String(),
		Email:           email,
		EmailVerifiedAt: &now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.db.Create(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

func (s *Store) GetUserByEmail(email string) (*User, error) {
	var user User
	if err := s.db.Where("email = ?", email).First(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

func (s *Store) GetUserByID(id string) (*User, error) {
	var user User
	if err := s.db.First(&user, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

// UpdateUserLastActiveOrg flips users.last_active_org_id so a
// reconnect lands the user back in the org they were last operating
// in. No-op when orgID matches the existing value.
func (s *Store) UpdateUserLastActiveOrg(userID string, orgID string) error {
	return s.db.Model(&User{}).Where("id = ?", userID).Updates(map[string]any{
		"last_active_org_id": orgID,
		"updated_at":         time.Now(),
	}).Error
}

// AdvanceOnboardingStep is a compare-and-swap on onboarding_step.
// Returns true when the swap succeeded; false when the current step
// didn't match `expected` (concurrent advance from another client,
// or a stale request). Idempotent: a request for a step the user is
// already past returns false but is otherwise harmless.
func (s *Store) AdvanceOnboardingStep(userID, expected, next string) (bool, error) {
	res := s.db.Model(&User{}).
		Where("id = ? AND onboarding_step = ?", userID, expected).
		Updates(map[string]any{
			"onboarding_step": next,
			"updated_at":      time.Now(),
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// --- Magic Links ---
//
// Tokens live plaintext in the email we send and in the URL the user
// clicks; in Postgres we only ever keep the sha256 hash. A DB dump
// therefore can't be turned into a login — an attacker would need a
// sha256 preimage for a 256-bit random value.

// hashMagicLinkToken returns the lookup value we store in magic_links.token.
func hashMagicLinkToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (s *Store) CreateMagicLink(email string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	raw := hex.EncodeToString(b)
	link := MagicLink{
		Email:     email,
		Token:     hashMagicLinkToken(raw),
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}
	if err := s.db.Create(&link).Error; err != nil {
		return "", err
	}
	s.rawMagicLinkMu.Lock()
	if s.rawMagicLinkTokens == nil {
		s.rawMagicLinkTokens = make(map[string]string)
	}
	s.rawMagicLinkTokens[email] = raw
	s.rawMagicLinkMu.Unlock()
	return raw, nil
}

// PeekMagicLink returns the email on a valid/unused/unexpired token
// *without* consuming it. Callers that only need to branch on the
// associated email (no state mutations) may use this. Callers that
// mutate state — create user, flip EmailVerifiedAt, accept invite —
// must go through ConsumeMagicLink so Postgres atomically decides
// which concurrent request wins.
func (s *Store) PeekMagicLink(token string) (string, error) {
	hash := hashMagicLinkToken(token)
	var link MagicLink
	if err := s.db.Where("token = ? AND used = ? AND expires_at > ?", hash, false, time.Now()).First(&link).Error; err != nil {
		return "", fmt.Errorf("invalid or expired magic link")
	}
	return link.Email, nil
}

// ConsumeMagicLink validates and marks the token as used in a single
// atomic UPDATE ... RETURNING — two concurrent requests can't both win
// because Postgres serializes the row update. Returns the email on
// success, "" and an error on invalid/expired/already-used.
func (s *Store) ConsumeMagicLink(token string) (string, error) {
	hash := hashMagicLinkToken(token)
	var email string
	res := s.db.Raw(`
		UPDATE magic_links
		   SET used = true
		 WHERE token = ? AND used = false AND expires_at > now()
		RETURNING email`, hash).Scan(&email)
	if res.Error != nil {
		return "", fmt.Errorf("consume magic link: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return "", fmt.Errorf("invalid or expired magic link")
	}
	return email, nil
}

// GetLastMagicLinkToken returns the most recently issued raw token for
// an email. Test-only: reads from an in-process map populated by
// CreateMagicLink, since the raw token is never persisted.
func (s *Store) GetLastMagicLinkToken(email string) (string, error) {
	s.rawMagicLinkMu.Lock()
	defer s.rawMagicLinkMu.Unlock()
	tok, ok := s.rawMagicLinkTokens[email]
	if !ok {
		return "", fmt.Errorf("no magic link for %s", email)
	}
	return tok, nil
}

// --- Team Members ---
//
// Membership itself is owned by saaskit/orgs: who belongs to an org, at what
// role, and where they are in the invite lifecycle. The methods below are the
// weft-facing names for that, delegating to s.Orgs.
//
// The tenant-scoping callbacks in tenant.go do not cover these — orgs runs on
// database/sql, beneath gorm, so it never sees them. Each method therefore
// passes org_id explicitly, which is what orgs' API requires anyway; there is
// no unscoped form to reach for by accident.
//
// What stays weft's: OrgPermission (the binmgr/cronmgr/configmgr bits) and
// NamespacePermission. orgs decides whether you are a member; weft decides
// what a member may touch.

// ListTeamMembers returns active and pending members (not removed), oldest
// first.
func (s *Store) ListTeamMembers(orgID string) ([]TeamMember, error) {
	return s.Orgs.Members(context.Background(), orgID)
}

// InviteTeamMember records a pending membership for an address that may not
// have an account yet. invitedBy is the inviting user's id.
func (s *Store) InviteTeamMember(orgID, email string, role orgs.Role, invitedBy string) (*TeamMember, error) {
	return s.Orgs.Invite(context.Background(), orgID, email, role, invitedBy)
}

// AddTeamMemberWithEmail attaches an account weft already knows to the org,
// active immediately — the signup and admin-provision paths, where there is
// nothing to invite because the account exists.
//
// The address is always passed: weft renders /etc/passwd from it and keys
// adduser/deluser by it, so a membership without one would be invisible to
// half the team surface.
func (s *Store) AddTeamMemberWithEmail(orgID, userID, email string, role orgs.Role) error {
	return s.Orgs.AddMemberWithEmail(context.Background(), orgID, userID, email, role)
}

// GetTeamMemberByEmail finds a non-removed member by email.
func (s *Store) GetTeamMemberByEmail(orgID string, email string) (*TeamMember, error) {
	m, err := s.Orgs.MemberByEmail(context.Background(), orgID, email)
	return m, asRecordNotFound(err)
}

// GetTeamMemberByUserID finds a non-removed member by (user, org).
// This is the canonical "is this user a member of this org" lookup; the
// auth middleware uses it to validate X-Active-Org against the user's
// memberships.
func (s *Store) GetTeamMemberByUserID(orgID string, userID string) (*TeamMember, error) {
	m, err := s.Orgs.Member(context.Background(), orgID, userID)
	return m, asRecordNotFound(err)
}

// ListUserOrgs returns every org a user actively belongs to, oldest first —
// the org switcher's backing query and the login flow's default-org pick.
//
// This replaces the old ListUserMemberships + Preload("Organization"): callers
// wanted the org's name, and orgs answers that directly rather than handing
// back memberships to walk associations through.
func (s *Store) ListUserOrgs(userID string) ([]orgs.OrgMembership, error) {
	return s.Orgs.OrgsFor(context.Background(), userID)
}

// GetPendingTeamMemberByEmail finds a pending invite by email across all orgs.
// The invitee isn't in any org context yet — the invite is what says which org
// they're joining.
func (s *Store) GetPendingTeamMemberByEmail(email string) (*TeamMember, error) {
	m, err := s.Orgs.PendingInviteByEmail(context.Background(), email)
	return m, asRecordNotFound(err)
}

// asRecordNotFound maps orgs' absence sentinel onto gorm's. Every caller in
// this store — and every `errors.Is(err, gorm.ErrRecordNotFound)` written
// against it — already means one thing by "no such row", and adopting orgs
// should not make that phrase ambiguous depending on which table you asked
// about.
func asRecordNotFound(err error) error {
	if errors.Is(err, orgs.ErrNotFound) {
		return gorm.ErrRecordNotFound
	}
	return err
}

// AcceptTeamMember binds a pending membership to a user and activates it.
func (s *Store) AcceptTeamMember(id string, userID string) error {
	_, err := s.Orgs.AcceptByID(context.Background(), id, userID)
	return err
}

// RemoveTeamMemberByEmail revokes a membership by address, which is the only
// way to withdraw an invite nobody has accepted.
func (s *Store) RemoveTeamMemberByEmail(orgID, email string) error {
	return s.Orgs.RemoveInvite(context.Background(), orgID, email)
}

// CountTeamMembers returns the count of active + pending members — the number
// a seat limit is checked against, so an outstanding invite holds its seat.
func (s *Store) CountTeamMembers(orgID string) (int, error) {
	return s.Orgs.CountMembers(context.Background(), orgID)
}

// SetUserName updates a user's free-text display name. Used for
// `ls -al`'s owner column and other places where a friendlier label
// than email-local-part is preferred. Pass "" to clear.
func (s *Store) SetUserName(userID, name string) error {
	return s.db.Model(&User{}).Where("id = ?", userID).Updates(map[string]any{
		"name":       name,
		"updated_at": time.Now(),
	}).Error
}

// GetUsersByIDs resolves a batch of user IDs to user rows. IDs not
// found are silently dropped from the result. Used by the
// ls -al renderer to look up display names for a directory listing
// without N+1 queries.
func (s *Store) GetUsersByIDs(ids []string) (map[string]User, error) {
	out := map[string]User{}
	if len(ids) == 0 {
		return out, nil
	}
	var users []User
	if err := s.db.Where("id IN ?", ids).Find(&users).Error; err != nil {
		return nil, err
	}
	for _, u := range users {
		out[u.ID] = u
	}
	return out, nil
}
