package hostedapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"gorm.io/gorm"

	weftapi "github.com/graybeardcorp/weft/api"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/hosted"
)

// --- Team management ---

func (p *product) handleListTeam(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)
	members, err := p.Store().ListTeamMembers(session.Org())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	count, _ := p.Store().CountTeamMembers(session.Org())
	tier := store.TierTrial
	if sub, _ := p.Store().ActiveSubscription(session.Org()); sub != nil {
		tier = store.Tier(sub.Tier)
	}
	orgName := session.ActiveCustomerName
	if cust, err := p.Store().GetOrgByName(session.ActiveCustomerName); err == nil && cust.DisplayName != "" {
		orgName = cust.DisplayName
	}
	json.NewEncoder(w).Encode(map[string]any{
		"members":     members,
		"seats_used":  count,
		"seats_limit": store.SeatLimit(tier),
		"org_name":    orgName,
	})
}

func (p *product) handleInviteTeamMember(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)

	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Email == "" {
		http.Error(w, "email required", http.StatusBadRequest)
		return
	}

	member, err := p.inviteMember(r.Context(), session, req.Email)
	if err != nil {
		if ie, ok := err.(*inviteErr); ok {
			http.Error(w, ie.msg, ie.status)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(member)
}

func (p *product) handleRemoveTeamMember(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}

	if p.Authorize(r.Context(), session.Caller(), hosted.ActionManageTeam, tenancy.Resource{}) != nil {
		http.Error(w, "only the owner can remove members", http.StatusForbidden)
		return
	}

	target, err := p.Store().GetTeamMemberByID(session.Org(), id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		http.Error(w, "member not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if target.Email == session.Email {
		http.Error(w, "cannot remove yourself", http.StatusBadRequest)
		return
	}

	if err := p.Store().RemoveMember(session.Org(), target.Email); err != nil {
		if errors.Is(err, store.ErrOwnerImmutable) {
			http.Error(w, "owners cannot be removed", http.StatusForbidden)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Membership is removed; user record itself is unchanged (the user
	// may belong to other orgs).
	w.WriteHeader(http.StatusNoContent)
}

func (p *product) handleUpdateTeamName(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)

	if p.Authorize(r.Context(), session.Caller(), hosted.ActionManageTeam, tenancy.Resource{}) != nil {
		http.Error(w, "only the owner can rename the team", http.StatusForbidden)
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	if err := p.Store().UpdateOrgDisplayName(session.ActiveCustomerName, req.Name); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"name": req.Name})
}

func (p *product) handleSetNamespacePermission(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)
	if p.Authorize(r.Context(), session.Caller(), hosted.ActionManageTeam, tenancy.Resource{}) != nil {
		http.Error(w, "forbidden: owner only", http.StatusForbidden)
		return
	}

	nsName := r.PathValue("name")
	userID := r.PathValue("userId")
	if nsName == "" || userID == "" {
		http.Error(w, "name and userId required", http.StatusBadRequest)
		return
	}

	custID, ok := weftapi.ActiveOrg(w, session.Caller())
	if !ok {
		return
	}
	ns, err := p.Store().GetNamespace(custID, nsName)
	if err != nil {
		http.Error(w, "namespace not found", http.StatusNotFound)
		return
	}

	var req store.NamespacePermission
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.UserID = userID
	req.NamespaceID = ns.ID

	if err := p.Store().UpsertNamespacePermission(&req); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleGetPermissions returns the active org's TeamPermsDoc — what
// every non-owner member can do across /bin /cron.d /etc and per-
// namespace. Readable by every authenticated org member (the same
// transparency as `cat /etc/passwd` in the shell). Backs the
// dashboard's Members grid.
func (p *product) handleGetPermissions(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)
	doc, err := p.Store().BuildTeamPerms(session.Org())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

// handleSetPermissions backs the dashboard's Members grid — the JSON
// twin of the shell's adduser/deluser, but whole-doc rather than one
// grant at a time. Owner-only; validate-then-upsert. Validation errors
// come back as 400 with a readable message so the UI can surface them.
func (p *product) handleSetPermissions(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)
	if p.Authorize(r.Context(), session.Caller(), hosted.ActionManageTeam, tenancy.Resource{}) != nil {
		http.Error(w, "forbidden: owner only", http.StatusForbidden)
		return
	}
	var doc store.TeamPermsDoc
	if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := p.Store().ApplyTeamPerms(session.Org(), doc); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
