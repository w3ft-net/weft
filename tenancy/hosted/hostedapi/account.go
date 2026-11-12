package hostedapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/graybeardcorp/weft/coordinator/onboarding"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
	weftsdk "github.com/w3ft-net/weft-go"
	"gorm.io/gorm"
)

// --- Auth handlers ---

func (p *product) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Email == "" {
		http.Error(w, "email required", http.StatusBadRequest)
		return
	}

	// Always return 200 regardless of whether the account exists.
	// Leaking existence is an enumeration vector.
	// Only an existing account gets a login link. A pending invitee has
	// their invite link, a prospective customer signs up; a login link is
	// neither of those.
	if user, _ := p.Store().GetUserByEmail(req.Email); user == nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		return
	}

	token, err := p.Store().CreateMagicLink(req.Email)
	if err != nil {
		weftsdk.Infof(r.Context(), "login: create magic link for %s: %v", req.Email, err)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		return
	}

	verifyURL := fmt.Sprintf("%s/verify?token=%s", p.BaseURL(), token)
	if err := SendMagicLinkEmail(r.Context(), p.email, req.Email, verifyURL); err != nil {
		weftsdk.Infof(r.Context(), "login: send magic link to %s: %v", req.Email, err)
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (p *product) handleSignup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email   string `json:"email"`
		OrgName string `json:"org_name"`
		Tier    string `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	email := store.NormalizeEmail(req.Email)
	orgName := store.NormalizeOrgName(req.OrgName)
	if email == "" || orgName == "" || req.Tier == "" {
		http.Error(w, "email, org_name, and tier are required", http.StatusBadRequest)
		return
	}

	// Bolt is quoted per customer, so there is no public checkout for it.
	if p.tierPurchasable == nil || !p.tierPurchasable(store.Tier(req.Tier)) || store.Tier(req.Tier) == store.TierBolt {
		http.Error(w, "invalid tier", http.StatusBadRequest)
		return
	}

	// These checks are for the form's benefit; the verify step enforces
	// both again, since nothing is reserved in between.
	if existing, _ := p.Store().GetUserByEmail(email); existing != nil {
		http.Error(w, "account already exists", http.StatusConflict)
		return
	}
	if existing, _ := p.Store().GetOrgByName(orgName); existing != nil {
		http.Error(w, "organization name already taken", http.StatusConflict)
		return
	}

	// Signup creates nothing durable and returns no session: the address is
	// unproven. The pending org name and tier ride on the emailed link, and
	// following it (handleVerify) is what creates the account.
	token, err := p.Store().IssueMagicLink(&store.MagicLink{
		Email:   email,
		Purpose: store.MagicLinkSignup,
		OrgName: orgName,
		Tier:    req.Tier,
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	verifyURL := fmt.Sprintf("%s/verify?token=%s", p.BaseURL(), token)
	if err := SendMagicLinkEmail(r.Context(), p.email, email, verifyURL); err != nil {
		weftsdk.Errorf(r.Context(), "signup: failed to send verification email to %s: %v", email, err)
		http.Error(w, "failed to send email", http.StatusBadGateway)
		return
	}

	json.NewEncoder(w).Encode(map[string]any{
		"status": "verification_sent",
		"email":  email,
		"tier":   req.Tier,
	})
}

// handleResendVerification re-sends the verification email for the
// authenticated caller. No-op (returns 200) for users already verified so
// the frontend can call it unconditionally from a "resend" button without
// worrying about state. The email send itself is best-effort — we log but
// don't fail if Sendgrid rejects.
func (p *product) handleResendVerification(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)
	user, err := p.Store().GetUserByID(session.UserID)
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	if user.EmailVerifiedAt != nil {
		json.NewEncoder(w).Encode(map[string]any{"status": "already_verified"})
		return
	}

	token, err := p.Store().CreateMagicLink(user.Email)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	verifyURL := fmt.Sprintf("%s/verify?token=%s", p.BaseURL(), token)
	if err := SendMagicLinkEmail(r.Context(), p.email, user.Email, verifyURL); err != nil {
		weftsdk.Errorf(r.Context(), "resend-verification: failed to send to %s: %v", user.Email, err)
		http.Error(w, "failed to send email", http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"status": "sent"})
}

// handleWelcomeDismiss flips the caller's HasSeenWelcome flag to true.
// Idempotent — re-dismiss returns 200 without touching UpdatedAt. The
// frontend calls this when the welcome modal closes; subsequent logins
// skip the modal because /api/auth/me reports has_seen_welcome: true.
func (p *product) handleWelcomeDismiss(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)
	if err := p.Store().DB().Model(&store.User{}).
		Where("id = ? AND has_seen_welcome = FALSE", session.UserID).
		Updates(map[string]any{
			"has_seen_welcome": true,
			"updated_at":       time.Now(),
		}).Error; err != nil {
		weftsdk.Infof(r.Context(), "welcome-dismiss: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
}

// handleVerify redeems a login or signup link. Which one it is comes from
// the link, never from what rows exist: a login link signs in an existing
// account, a signup link creates the account it was issued for. Invite links
// belong to handleAcceptInvite and are refused here.
//
// Redemption is one transaction that starts by consuming the link, so a
// failure at any step leaves nothing behind and the link still usable.
func (p *product) handleVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}

	var (
		user       *store.User
		created    bool
		signupTier string
	)
	err := p.Store().InTx(func(tx *store.Store) error {
		link, err := tx.ConsumeMagicLink(req.Token)
		if err != nil {
			return err
		}
		switch link.Purpose {
		case store.MagicLinkLogin:
			user, err = tx.GetUserByEmail(link.Email)
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return store.ErrMagicLinkInvalid
			}
			if err != nil {
				return err
			}
			return tx.VerifyUserEmail(user)
		case store.MagicLinkSignup:
			signupTier = link.Tier
			user, created, err = completeSignup(tx, link)
			return err
		default:
			return store.ErrMagicLinkInvalid
		}
	})
	if err != nil {
		p.writeLinkError(w, r, "verify", err)
		return
	}

	if created {
		if err := p.grantLoomMembership(r.Context(), user.ID, user.Email); err != nil {
			weftsdk.Infof(r.Context(), "verify: grant Loom membership for %s: %v", user.Email, err)
		}
	}
	p.writeSession(w, r, user, map[string]any{"signup_tier": signupTier})
}

// completeSignup creates what a signup link promised: a verified owner of a
// brand-new org, awaiting a card for the plan they chose. If the address already has an account (say it
// accepted an invite since signing up) the link only proves the address.
func completeSignup(tx *store.Store, link *store.MagicLink) (user *store.User, created bool, err error) {
	user, err = tx.GetUserByEmail(link.Email)
	if err == nil {
		return user, false, tx.VerifyUserEmail(user)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}

	org, err := tx.CreateOrg(link.OrgName)
	if err != nil {
		return nil, false, err
	}
	if user, err = tx.CreateVerifiedUser(link.Email); err != nil {
		return nil, false, err
	}
	// The card comes first. The org gets no active subscription, and so no
	// ingest, queries or shell, until Stripe holds a payment method for the
	// plan the visitor chose; the setup-intent step attaches the Stripe
	// subscription and the webhook activates this row.
	if err := tx.CreateSubscription(&store.Subscription{
		ID:       uuid.New().String(),
		OrgID:    org.ID,
		Tier:     link.Tier,
		Status:   "pending_payment",
		StartsAt: time.Now(),
	}); err != nil {
		return nil, false, err
	}
	// The owner membership is what binds user to org.
	if err := tx.AddTeamMemberWithEmail(org.ID, user.ID, link.Email, store.RoleOwner); err != nil {
		return nil, false, err
	}
	if err := tx.UpdateUserLastActiveOrg(user.ID, org.ID); err != nil {
		return nil, false, err
	}
	return user, true, nil
}

// writeLinkError maps a failed link redemption to a response. Every way a
// link can be bad reads the same; only a taken org name is told apart, since
// the caller can act on it.
func (p *product) writeLinkError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, store.ErrMagicLinkInvalid):
		http.Error(w, store.ErrMagicLinkInvalid.Error(), http.StatusUnauthorized)
	case errors.Is(err, store.ErrOrgNameTaken):
		http.Error(w, "organization name already taken", http.StatusConflict)
	default:
		weftsdk.Errorf(r.Context(), "%s: %v", op, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// writeSession answers a successful redemption with a JWT and the same user
// shape /me returns — AuthContext seeds its user straight from this payload,
// and the welcome modal keys off onboarding_step and the orgs list it
// carries. extra is merged into the top level.
func (p *product) writeSession(w http.ResponseWriter, r *http.Request, user *store.User, extra map[string]any) {
	jwt, err := auth.Generate(user, p.secret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Reload so the response reflects the writes above (verified flag,
	// membership).
	if fresh, err := p.Store().GetUserByID(user.ID); err == nil {
		user = fresh
	}
	activeID, activeName := p.resolveActiveOrg(user)
	resp := map[string]any{
		"token": jwt,
		"user":  p.userResponse(r.Context(), user, activeID, activeName),
	}
	for k, v := range extra {
		resp[k] = v
	}
	json.NewEncoder(w).Encode(resp)
}

func (p *product) handleMe(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)

	user, err := p.Store().GetUserByID(session.UserID)
	if err != nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	// The tour's last step asks the user to pick their own org in the
	// selector. Choosing one is the graduation: remove Loom here, on the
	// first request scoped elsewhere, rather than trusting the browser.
	if user.OnboardingStep == "graduate" && session.ActiveCustomerName != LoomCustomerSlug {
		if advanced, err := p.Store().AdvanceOnboardingStep(user.ID, "graduate", "done"); err != nil {
			weftsdk.Infof(r.Context(), "onboarding: graduate %s on org switch: %v", user.ID, err)
		} else if advanced {
			if err := p.graduateOnboarding(r.Context(), user.ID); err != nil {
				weftsdk.Infof(r.Context(), "onboarding: graduate %s: %v", user.ID, err)
			}
			// graduateOnboarding pins the home org; the user chose this one.
			_ = p.Store().UpdateUserLastActiveOrg(user.ID, session.Org())
			if fresh, err := p.Store().GetUserByID(user.ID); err == nil {
				user = fresh
			}
		}
	}

	json.NewEncoder(w).Encode(p.userResponse(r.Context(), user, session.Org(), session.ActiveCustomerName))
}

// userResponse builds the canonical user object the SPA consumes. Both
// /api/auth/me (on reload) and /api/auth/verify (at login) return it, so
// AuthContext gets the same shape whichever it hits first. Keeping it in
// one place is the fix for a class of bug where the two disagreed: /verify
// used to return a 4-field subset with no onboarding_step, so the welcome
// modal — which gates on that field off the object AuthContext seeds from
// /verify — never rendered on a fresh comp account's first session.
//
// activeOrgID/Name are the org the response is scoped to: the
// middleware-resolved session org for /me, or the user's landing org for
// /verify. Tier and the orgs "active" flag are computed against it.
func (p *product) userResponse(ctx context.Context, user *store.User, activeOrgID store.OrgID, activeCustomerName string) map[string]any {
	// Tier and customer come from the resolved active org, not from a
	// user.CustomerName field that no longer exists.
	tier := store.Tier("none")
	pendingPaymentTier := ""
	if activeOrgID != 0 {
		if sub, _ := p.Store().ActiveSubscription(activeOrgID); sub != nil {
			tier = store.Tier(sub.Tier)
		} else if pending, _ := p.Store().PendingPaymentSubscription(activeOrgID); pending != nil {
			pendingPaymentTier = pending.Tier
		}
	}

	// Memberships drive the org switcher in the UI — return them so the
	// frontend can render a list without a second round-trip.
	userOrgs, _ := p.Store().ListUserOrgs(user.ID)
	orgs := make([]map[string]any, 0, len(userOrgs))
	for _, m := range userOrgs {
		orgs = append(orgs, map[string]any{
			"id":     m.ID,
			"name":   m.Name,
			"role":   m.Role,
			"active": m.ID == activeOrgID.String(),
		})
	}

	// pending_graduation_notice is the success banner the home org shows
	// after a Loom tour completion. It tracks the queued notice, so it
	// goes false once the notice has been delivered anywhere — including
	// in the shell, which drains the same row.
	pendingGraduation := len(p.pendingGraduationIDs(ctx, user.ID)) > 0

	resp := map[string]any{
		"id":                   user.ID,
		"email":                user.Email,
		"name":                 user.Name,
		"tier":                 tier,
		"pending_payment_tier": pendingPaymentTier,
		"customer":             activeCustomerName,
		"orgs":                 orgs,
		"email_verified":       user.EmailVerifiedAt != nil,
		"has_seen_welcome":     user.HasSeenWelcome,
		"onboarding_step":      user.OnboardingStep,
		"onboarding_dismissed": user.OnboardingDismissed,
		// onboarding_prompt is the lesson copy for the current
		// shell-* step. The frontend renders it verbatim — that
		// way the lesson list lives in one place (the Go package)
		// and the UI doesn't need updating each time we add a
		// lesson. onboarding_action is the suggested command; the
		// web shell injects it into the input so the user can just
		// hit enter.
		"is_superadmin":             user.IsSuperadmin,
		"onboarding_prompt":         onboarding.PromptFor(user.OnboardingStep),
		"onboarding_action":         onboarding.ActionFor(user.OnboardingStep),
		"pending_graduation_notice": pendingGraduation,
	}
	if user.EmailVerifiedAt == nil {
		resp["verification_deadline"] = user.CreatedAt.Add(auth.UnverifiedGracePeriod).Unix()
	}
	return resp
}

// resolveActiveOrg picks the org a just-verified user lands in.
// Verify runs before AuthMiddleware establishes a session, so there's no
// resolved active org yet — fall back to the user's last_active_customer
// (set by every account-creation path), then to their oldest membership.
func (p *product) resolveActiveOrg(user *store.User) (store.OrgID, string) {
	if user.LastActiveOrgID != nil {
		if c, err := p.Store().GetOrgByID(*user.LastActiveOrgID); err == nil && c != nil {
			return c.ID, c.Name
		}
	}
	if mems, _ := p.Store().ListUserOrgs(user.ID); len(mems) > 0 {
		if id, err := store.ParseOrgID(mems[0].ID); err == nil {
			return id, mems[0].Name
		}
	}
	return 0, ""
}

// --- Invite redemption ---

// handleAcceptInvite redeems an invite link: it joins the invitee to the one
// org the link names and nothing else. Same shape as handleVerify — one
// transaction that begins by consuming the link.
func (p *product) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}

	var (
		user    *store.User
		created bool
	)
	err := p.Store().InTx(func(tx *store.Store) error {
		link, err := tx.ConsumeMagicLink(req.Token)
		if err != nil {
			return err
		}
		if link.Purpose != store.MagicLinkInvite {
			return store.ErrMagicLinkInvalid
		}

		// Clicking the link proves the address, so the user is verified
		// whether they already had an account or not.
		user, err = tx.GetUserByEmail(link.Email)
		switch {
		case err == nil:
			if err := tx.VerifyUserEmail(user); err != nil {
				return err
			}
		case errors.Is(err, gorm.ErrRecordNotFound):
			if user, err = tx.CreateVerifiedUser(link.Email); err != nil {
				return err
			}
			created = true
		default:
			return err
		}

		member, err := tx.AcceptTeamMember(link.InviteID, user.ID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errInviteGone
		}
		if err != nil {
			return err
		}
		if !strings.EqualFold(member.Email, link.Email) {
			return store.ErrMagicLinkInvalid
		}
		orgID, err := store.ParseOrgID(member.OrgID)
		if err != nil {
			return err
		}
		return tx.UpdateUserLastActiveOrg(user.ID, orgID)
	})
	if errors.Is(err, errInviteGone) {
		http.Error(w, "this invite is no longer valid", http.StatusNotFound)
		return
	}
	if err != nil {
		p.writeLinkError(w, r, "accept invite", err)
		return
	}

	if created {
		if err := p.grantLoomMembership(r.Context(), user.ID, user.Email); err != nil {
			weftsdk.Infof(r.Context(), "accept invite: grant Loom membership for %s: %v", user.Email, err)
		}
	}
	p.writeSession(w, r, user, nil)
}

// errInviteGone: the invite the link names was withdrawn (or never existed).
var errInviteGone = errors.New("invite no longer valid")

// ── Device auth flow (CLI login) ─────────────────────────────────────

// handleDeviceInit creates a new device code for CLI authorization.
// POST /api/auth/device → {"device_code": "...", "user_code": "XXXX-XX", "expires_in": 600}
func (p *product) handleDeviceInit(w http.ResponseWriter, r *http.Request) {
	deviceBytes := make([]byte, 16)
	if _, err := rand.Read(deviceBytes); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	userBytes := make([]byte, 3)
	if _, err := rand.Read(userBytes); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	dc := store.DeviceCode{
		DeviceCode: fmt.Sprintf("%x", deviceBytes),
		UserCode:   fmt.Sprintf("%X-%X", userBytes[:2], userBytes[2:]),
		ExpiresAt:  time.Now().Add(10 * time.Minute),
		CreatedAt:  time.Now(),
	}
	if err := p.Store().DB().Create(&dc).Error; err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"device_code": dc.DeviceCode,
		"user_code":   dc.UserCode,
		"expires_in":  600,
	})
}

// handleDevicePoll checks if a device code has been approved.
// POST /api/auth/device/poll {"device_code": "..."}
// Returns {"status": "pending"} or {"status": "approved", "token": "jwt..."}
func (p *product) handleDevicePoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeviceCode == "" {
		http.Error(w, "device_code required", http.StatusBadRequest)
		return
	}

	var pollDC store.DeviceCode
	var userID string
	if err := p.Store().DB().Where("device_code = ?", req.DeviceCode).First(&pollDC).Error; err != nil {
		http.Error(w, "invalid device code", http.StatusGone)
		return
	}
	if time.Now().After(pollDC.ExpiresAt) {
		http.Error(w, "device code expired", http.StatusGone)
		return
	}
	if pollDC.UserID != nil {
		userID = *pollDC.UserID
	}
	if userID == "" {
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
		return
	}

	user, err := p.Store().GetUserByID(userID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	jwt, err := auth.Generate(user, p.secret)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// The CLI persists this so subsequent calls send X-Active-Org. Resolve
	// from last_active_org_id, falling back to the oldest membership.
	defaultOrg := ""
	if userOrgs, _ := p.Store().ListUserOrgs(user.ID); len(userOrgs) > 0 {
		defaultOrg = userOrgs[0].Name
		if user.LastActiveOrgID != nil {
			for _, m := range userOrgs {
				if m.ID == user.LastActiveOrgID.String() {
					defaultOrg = m.Name
					break
				}
			}
		}
	}

	json.NewEncoder(w).Encode(map[string]any{
		"status":   "approved",
		"token":    jwt,
		"email":    user.Email,
		"customer": defaultOrg,
	})
}

// handleDeviceApprove approves a device code (requires auth — user clicks in browser).
// POST /api/auth/device/approve {"user_code": "XXXX-XX"}
func (p *product) handleDeviceApprove(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)

	var req struct {
		UserCode string `json:"user_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserCode == "" {
		http.Error(w, "user_code required", http.StatusBadRequest)
		return
	}

	result := p.Store().DB().Model(&store.DeviceCode{}).
		Where("user_code = ? AND user_id IS NULL AND expires_at > ?", req.UserCode, time.Now()).
		Update("user_id", session.UserID)
	if result.RowsAffected == 0 {
		http.Error(w, "device code not found or expired", http.StatusNotFound)
		return
	}
	if result.Error != nil {
		http.Error(w, result.Error.Error(), http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "approved"})
}
