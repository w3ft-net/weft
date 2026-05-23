package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
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
		return 365 * 24 * time.Hour
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
// / 1 TB" has always been decimal in customer-speak; the internal
// constant was 1024³-based (GiB) before this, disagreeing by 7.4%.
// See docs/plans/stripe.md for the alignment rationale.
func VolumeAllowance(tier Tier) int64 {
	switch tier {
	case TierSwatch:
		return 10 * 1_000_000_000
	case TierYard:
		return 100 * 1_000_000_000
	case TierBolt:
		return 1_000_000_000_000
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
		return 50 // $0.50
	default:
		return 0
	}
}

// User represents a weft account. A single user can be a member of
// multiple customers (orgs) via team_members rows; the User row itself
// no longer carries a customer pointer. LastActiveCustomerID records
// where the user last operated so reconnects land in the same place.
type User struct {
	ID                 string `gorm:"primaryKey"`
	Email              string `gorm:"not null;uniqueIndex:idx_users_email"`
	Name               string
	StripeCustomerID   string
	CanCreateNamespace bool `gorm:"not null;default:false"`
	IsSuperadmin       bool `gorm:"not null;default:false"`
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
	// LastActiveCustomerID is the customer the user was most recently
	// scoped to. The middleware uses it as the default active org when
	// the request omits the X-Active-Org header. Nullable because new
	// users haven't picked one yet.
	LastActiveCustomerID *uint
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

// TeamMember tracks membership of a user (or pending invite) in a
// customer. CustomerID is the FK boundary; queries that need to scope
// by customer should join on it rather than on a name string.
type TeamMember struct {
	ID         string   `gorm:"primaryKey"`
	CustomerID uint     `gorm:"not null;index;uniqueIndex:idx_team_members_customer_user"`
	Customer   Customer `gorm:"foreignKey:CustomerID;constraint:OnDelete:CASCADE"`
	Email      string   `gorm:"not null"`
	UserID     *string  `gorm:"index;uniqueIndex:idx_team_members_customer_user"`
	Role       string   `gorm:"not null;default:member"`  // "owner" or "member"
	Status     string   `gorm:"not null;default:pending"` // "pending", "active", "removed"
	InvitedBy  string
	InvitedAt  time.Time
	AcceptedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

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

// UpdateUserLastActiveCustomer flips users.last_active_customer_id so a
// reconnect lands the user back in the org they were last operating
// in. No-op when customerID matches the existing value.
func (s *Store) UpdateUserLastActiveCustomer(userID string, customerID uint) error {
	return s.db.Model(&User{}).Where("id = ?", userID).Updates(map[string]any{
		"last_active_customer_id": customerID,
		"updated_at":              time.Now(),
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

// ListTeamMembers returns active and pending members (not removed).
func (s *Store) ListTeamMembers(customerID uint) ([]TeamMember, error) {
	var members []TeamMember
	err := s.scoped(customerID).Where("status != ?", "removed").
		Order("role ASC, created_at ASC").Find(&members).Error
	return members, err
}

// CreateTeamMember inserts a new team member record.
func (s *Store) CreateTeamMember(m *TeamMember) error {
	return s.scoped(m.CustomerID).Create(m).Error
}

// GetTeamMemberByEmail finds a non-removed member by email.
func (s *Store) GetTeamMemberByEmail(customerID uint, email string) (*TeamMember, error) {
	var m TeamMember
	err := s.scoped(customerID).Where("email = ? AND status != ?", email, "removed").
		First(&m).Error
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// GetTeamMemberByUserID finds a non-removed member by (user, customer).
// This is the canonical "is this user a member of this org" lookup; the
// auth middleware uses it to validate X-Active-Org against the user's
// memberships.
func (s *Store) GetTeamMemberByUserID(customerID uint, userID string) (*TeamMember, error) {
	var m TeamMember
	err := s.scoped(customerID).Where("user_id = ? AND status != ?", userID, "removed").
		First(&m).Error
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ListUserMemberships returns every active membership for a user, with
// the Customer eagerly loaded so callers can render org names without
// a second round-trip. Used by the org-switcher and by login flows that
// need to pick a default active customer.
func (s *Store) ListUserMemberships(userID string) ([]TeamMember, error) {
	var members []TeamMember
	// A user's memberships span every org they belong to — inherently
	// cross-tenant, so it bypasses the per-customer scope.
	err := s.crossTenant().Preload("Customer").
		Where("user_id = ? AND status = ?", userID, "active").
		Order("created_at ASC").Find(&members).Error
	return members, err
}

// GetPendingTeamMemberByEmail finds a pending invite by email (across all customers).
func (s *Store) GetPendingTeamMemberByEmail(email string) (*TeamMember, error) {
	var m TeamMember
	// Invite-acceptance flow: the invitee isn't in any org context yet,
	// so the pending lookup spans customers.
	err := s.crossTenant().Where("email = ? AND status = ?", email, "pending").First(&m).Error
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// AcceptTeamMember marks a pending member as active.
func (s *Store) AcceptTeamMember(id string, userID string) error {
	now := time.Now()
	return s.crossTenant().Model(&TeamMember{}).Where("id = ?", id).Updates(map[string]any{
		"user_id":     userID,
		"status":      "active",
		"accepted_at": &now,
		"updated_at":  now,
	}).Error
}

// CountTeamMembers returns the count of active + pending members.
func (s *Store) CountTeamMembers(customerID uint) (int, error) {
	var count int64
	err := s.scoped(customerID).Model(&TeamMember{}).
		Where("status != ?", "removed").
		Count(&count).Error
	return int(count), err
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

// SetCanCreateNamespace flips the namespace-creation flag for a user.
// Used by the demo seed and the (eventual) admin UI for promoting
// teammates beyond default permissions.
func (s *Store) SetCanCreateNamespace(userID string, allowed bool) error {
	return s.db.Model(&User{}).Where("id = ?", userID).Updates(map[string]any{
		"can_create_namespace": allowed,
		"updated_at":           time.Now(),
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
