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
		return 5
	case TierYard:
		return 10
	case TierBolt:
		return 50
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

// User represents a weft user. A user belongs to a customer (org).
// The composite unique index allows a user to exist under multiple customers.
type User struct {
	ID                 string   `gorm:"primaryKey"`
	Email              string   `gorm:"not null;uniqueIndex:idx_user_email_customer"`
	CustomerID         uint     `gorm:"not null;uniqueIndex:idx_user_email_customer"`
	Customer           Customer `gorm:"foreignKey:CustomerID;constraint:OnDelete:CASCADE"`
	Name               string
	StripeCustomerID   string
	CanCreateNamespace bool `gorm:"not null;default:false"`
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
	CreatedAt       time.Time
	UpdatedAt       time.Time

	// Deprecated: use CustomerID. Kept temporarily for migration.
	CustomerName string `gorm:"-"` // excluded from DB, computed on read
}

// MagicLink is a one-time-use login token.
type MagicLink struct {
	ID        uint   `gorm:"primaryKey"`
	Email     string `gorm:"not null"`
	Token     string `gorm:"uniqueIndex;not null"`
	ExpiresAt time.Time
	Used      bool `gorm:"not null;default:false"`
}

// SavedQuery is a user's bookmarked pipeline.
type SavedQuery struct {
	ID        string `gorm:"primaryKey"`
	UserID    string `gorm:"not null;index"`
	Name      string `gorm:"not null"`
	Query     string `gorm:"not null"` // the pipeline text
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TeamMember tracks membership in a customer team.
type TeamMember struct {
	ID           string  `gorm:"primaryKey"`
	CustomerName string  `gorm:"not null;index"`
	Email        string  `gorm:"not null"`
	UserID       *string `gorm:"index"`
	Role         string  `gorm:"not null;default:member"`  // "owner" or "member"
	Status       string  `gorm:"not null;default:pending"` // "pending", "active", "removed"
	InvitedBy    string
	InvitedAt    time.Time
	AcceptedAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
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

// CreateUser inserts a new user with EmailVerifiedAt == nil. Callers that
// know the email has already been proven (e.g. brand-new magic-link flow)
// should use CreateVerifiedUser instead.
func (s *Store) CreateUser(email string, customerID uint) (*User, error) {
	now := time.Now()
	user := User{
		ID:         uuid.New().String(),
		Email:      email,
		CustomerID: customerID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.db.Create(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

// CreateVerifiedUser is like CreateUser but marks EmailVerifiedAt = now.
// Used by the magic-link signup path where ownership is proven in a single
// round-trip (the user wouldn't receive the email otherwise).
func (s *Store) CreateVerifiedUser(email string, customerID uint) (*User, error) {
	now := time.Now()
	user := User{
		ID:              uuid.New().String(),
		Email:           email,
		CustomerID:      customerID,
		EmailVerifiedAt: &now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.db.Create(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

// MarkUserVerified flips EmailVerifiedAt to now() for an existing user.
// No-op if already verified.
func (s *Store) MarkUserVerified(userID string) error {
	now := time.Now()
	return s.db.Model(&User{}).
		Where("id = ? AND email_verified_at IS NULL", userID).
		Updates(map[string]any{
			"email_verified_at": &now,
			"updated_at":        now,
		}).Error
}

// MarkWelcomeSeen flips HasSeenWelcome to true. Idempotent — re-dismiss
// doesn't touch UpdatedAt if the flag is already set.
func (s *Store) MarkWelcomeSeen(userID string) error {
	return s.db.Model(&User{}).
		Where("id = ? AND has_seen_welcome = FALSE", userID).
		Updates(map[string]any{
			"has_seen_welcome": true,
			"updated_at":       time.Now(),
		}).Error
}

// DeleteExpiredUnverifiedUsers removes users whose email was never verified
// and whose CreatedAt is older than cutoff. Cascades via FK constraints to
// the customer, subscription, team member, tokens, sources, etc. Returns
// how many users were swept.
func (s *Store) DeleteExpiredUnverifiedUsers(cutoff time.Time) (int64, error) {
	result := s.db.
		Where("email_verified_at IS NULL AND created_at < ?", cutoff).
		Delete(&User{})
	return result.RowsAffected, result.Error
}

func (s *Store) GetUserByEmail(email string) (*User, error) {
	var user User
	if err := s.db.Preload("Customer").Where("email = ?", email).First(&user).Error; err != nil {
		return nil, err
	}
	user.CustomerName = user.Customer.Name
	return &user, nil
}

// GetUserByEmailAndCustomer finds a user for a specific customer.
func (s *Store) GetUserByEmailAndCustomer(email string, customerID uint) (*User, error) {
	var user User
	if err := s.db.Preload("Customer").Where("email = ? AND customer_id = ?", email, customerID).First(&user).Error; err != nil {
		return nil, err
	}
	user.CustomerName = user.Customer.Name
	return &user, nil
}

func (s *Store) GetUserByID(id string) (*User, error) {
	var user User
	if err := s.db.Preload("Customer").First(&user, "id = ?", id).Error; err != nil {
		return nil, err
	}
	user.CustomerName = user.Customer.Name
	return &user, nil
}

func (s *Store) UpdateUserStripeCustomer(userID, stripeCustomerID string) error {
	return s.db.Model(&User{}).Where("id = ?", userID).Updates(map[string]any{
		"stripe_customer_id": stripeCustomerID,
		"updated_at":         time.Now(),
	}).Error
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
	s.rememberRawMagicLink(email, raw)
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

func (s *Store) rememberRawMagicLink(email, raw string) {
	s.rawMagicLinkMu.Lock()
	defer s.rawMagicLinkMu.Unlock()
	if s.rawMagicLinkTokens == nil {
		s.rawMagicLinkTokens = make(map[string]string)
	}
	s.rawMagicLinkTokens[email] = raw
}

// --- Saved Queries ---

func (s *Store) CreateSavedQuery(userID, name, query string) (*SavedQuery, error) {
	sq := SavedQuery{
		ID:        uuid.New().String(),
		UserID:    userID,
		Name:      name,
		Query:     query,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := s.db.Create(&sq).Error; err != nil {
		return nil, err
	}
	return &sq, nil
}

func (s *Store) ListSavedQueries(userID string) ([]SavedQuery, error) {
	var queries []SavedQuery
	err := s.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&queries).Error
	return queries, err
}

func (s *Store) DeleteSavedQuery(userID, queryID string) error {
	return s.db.Where("id = ? AND user_id = ?", queryID, userID).Delete(&SavedQuery{}).Error
}

// --- Team Members ---

// ListTeamMembers returns active and pending members (not removed).
func (s *Store) ListTeamMembers(customerName string) ([]TeamMember, error) {
	var members []TeamMember
	err := s.db.Where("customer_name = ? AND status != ?", customerName, "removed").
		Order("role ASC, created_at ASC").Find(&members).Error
	return members, err
}

// CreateTeamMember inserts a new team member record.
func (s *Store) CreateTeamMember(m *TeamMember) error {
	return s.db.Create(m).Error
}

// GetTeamMemberByEmail finds a non-removed member by email.
func (s *Store) GetTeamMemberByEmail(customerName, email string) (*TeamMember, error) {
	var m TeamMember
	err := s.db.Where("customer_name = ? AND email = ? AND status != ?", customerName, email, "removed").
		First(&m).Error
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// GetPendingTeamMemberByEmail finds a pending invite by email (across all customers).
func (s *Store) GetPendingTeamMemberByEmail(email string) (*TeamMember, error) {
	var m TeamMember
	err := s.db.Where("email = ? AND status = ?", email, "pending").First(&m).Error
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// GetTeamMemberByID finds a member by ID.
func (s *Store) GetTeamMemberByID(id string) (*TeamMember, error) {
	var m TeamMember
	if err := s.db.First(&m, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

// AcceptTeamMember marks a pending member as active.
func (s *Store) AcceptTeamMember(id string, userID string) error {
	now := time.Now()
	return s.db.Model(&TeamMember{}).Where("id = ?", id).Updates(map[string]any{
		"user_id":     userID,
		"status":      "active",
		"accepted_at": &now,
		"updated_at":  now,
	}).Error
}

// RemoveTeamMember sets a member's status to removed.
func (s *Store) RemoveTeamMember(id string) error {
	return s.db.Model(&TeamMember{}).Where("id = ?", id).Updates(map[string]any{
		"status":     "removed",
		"updated_at": time.Now(),
	}).Error
}

// CountTeamMembers returns the count of active + pending members.
func (s *Store) CountTeamMembers(customerName string) (int, error) {
	var count int64
	err := s.db.Model(&TeamMember{}).
		Where("customer_name = ? AND status != ?", customerName, "removed").
		Count(&count).Error
	return int(count), err
}

// UpdateUserCustomer sets the customer association for a user.
func (s *Store) UpdateUserCustomer(userID string, customerID uint) error {
	return s.db.Model(&User{}).Where("id = ?", userID).Updates(map[string]any{
		"customer_id": customerID,
		"updated_at":  time.Now(),
	}).Error
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
