package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
)

// ── Customer handlers ───────────────────────────────────────────────

// compTiers are the paid tiers a comp account may be granted. Trial is
// excluded on purpose — a comp is an out-of-band grant of a real plan,
// not a self-serve trial (which the signup flow, now retired, used to
// mint). Keep in sync with the tier <select> in the admin UI.
var compTiers = map[string]bool{
	string(store.TierSwatch): true,
	string(store.TierYard):   true,
	string(store.TierBolt):   true,
}

// handleAdminCreateCustomer provisions a fresh comp account: a customer
// (org), a verified owner user, the owner's team membership, and a comp
// subscription at the requested tier. This is the operator replacement
// for public signup (retired) — the owner is created email-verified so
// they aren't blocked by RequireEmailVerified, and gets a magic link
// (emailed best-effort; also returned so the operator can hand it off).
//
// Mirrors handleSignup's guards: 409 on an existing user email or a
// taken org name.
func (s *Server) handleAdminCreateCustomer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email   string `json:"email"`
		OrgName string `json:"org_name"`
		Tier    string `json:"tier"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	customerName := strings.ToLower(strings.TrimSpace(req.OrgName))
	if email == "" || customerName == "" {
		http.Error(w, "email and org_name are required", http.StatusBadRequest)
		return
	}
	if req.Tier == "" {
		req.Tier = string(store.TierBolt)
	}
	if !compTiers[req.Tier] {
		http.Error(w, "invalid tier (must be swatch, yard, or bolt)", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "comp-account"
	}

	if existing, _ := s.store.GetUserByEmail(email); existing != nil {
		http.Error(w, "account already exists", http.StatusConflict)
		return
	}
	if existing, _ := s.store.GetOrgByName(customerName); existing != nil {
		http.Error(w, "organization name already taken", http.StatusConflict)
		return
	}

	cust, err := s.store.FindOrCreateOrg(customerName)
	if err != nil {
		log.Printf("admin: create customer %q: %v", customerName, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Verified owner: an operator vouches for the address, and gating a
	// comp account behind an email round-trip is pure friction.
	user, err := s.store.CreateVerifiedUser(email)
	if err != nil {
		log.Printf("admin: create owner %q: %v", email, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := s.store.AddTeamMemberWithEmail(cust.ID, user.ID, email, store.RoleOwner); err != nil {
		log.Printf("admin: create owner membership for %q: %v", email, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := s.store.GrantCompSubscription(cust.ID, req.Tier, reason); err != nil {
		log.Printf("admin: grant comp for %q: %v", customerName, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Read-only seat in the Loom demo org, same as the retired signup
	// path did. The welcome/tour modal keys off this membership — without
	// it the comp owner lands on an empty dashboard and "Start the tour"
	// is disabled. Soft-fails if Loom isn't seeded (see grantLoomMembership).
	if err := s.grantLoomMembership(user.ID, email); err != nil {
		log.Printf("admin: grant Loom membership for %q: %v", email, err)
	}

	// Land the owner in their new org on next request.
	_ = s.store.UpdateUserLastActiveOrg(user.ID, cust.ID)

	// Magic link so the owner can log in. Best-effort email (dev mode has
	// no EmailConfig); the URL is returned regardless so the operator can
	// deliver it out of band.
	loginURL := ""
	if token, err := s.store.CreateMagicLink(email); err != nil {
		log.Printf("admin: create magic link for %q: %v", email, err)
	} else {
		loginURL = fmt.Sprintf("%s/verify?token=%s", s.baseURL, token)
		if err := SendMagicLinkEmail(s.email, email, loginURL); err != nil {
			log.Printf("admin: send login link to %q: %v", email, err)
		}
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"name":        cust.Name,
		"tier":        req.Tier,
		"comp_reason": reason,
		"owner_email": email,
		"login_url":   loginURL,
	})
}

func (s *Server) handleAdminListCustomers(w http.ResponseWriter, r *http.Request) {
	var customers []store.Organization
	err := s.store.DB().Order("name").Find(&customers).Error
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	type result struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Tier        string `json:"tier"`
		IsComp      bool   `json:"is_comp"`
		CompReason  string `json:"comp_reason,omitempty"`
		Status      string `json:"status"`
		CreatedAt   string `json:"created_at"`
	}
	out := make([]result, 0, len(customers))
	for _, c := range customers {
		r := result{
			ID:          c.ID,
			Name:        c.Name,
			DisplayName: c.DisplayName,
			CreatedAt:   c.CreatedAt.Format(time.RFC3339),
		}
		if sub, _ := s.store.ActiveSubscription(c.ID); sub != nil {
			r.Tier = sub.Tier
			r.IsComp = sub.IsComp()
			r.CompReason = sub.CompReason
			r.Status = sub.Status
		} else {
			r.Tier = "none"
			r.Status = "inactive"
		}
		out = append(out, r)
	}
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleAdminGetCustomer(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cust, err := s.store.GetOrgByName(name)
	if err != nil {
		http.Error(w, "customer not found", http.StatusNotFound)
		return
	}

	nss, _ := s.store.ListNamespaces(cust.ID)
	type nsInfo struct {
		ID   uint   `json:"id"`
		Name string `json:"name"`
	}
	namespaces := make([]nsInfo, len(nss))
	for i, ns := range nss {
		namespaces[i] = nsInfo{ID: ns.ID, Name: ns.Name}
	}

	var sub *store.Subscription
	sub, _ = s.store.ActiveSubscription(cust.ID)

	members, _ := s.store.ListTeamMembers(cust.ID)
	type memberInfo struct {
		Email  string `json:"email"`
		Role   string `json:"role"`
		Status string `json:"status"`
	}
	team := make([]memberInfo, 0, len(members))
	for _, m := range members {
		team = append(team, memberInfo{Email: m.Email, Role: string(m.Role), Status: string(m.Status)})
	}

	resp := map[string]any{
		"id":           cust.ID,
		"name":         cust.Name,
		"display_name": cust.DisplayName,
		"namespaces":   namespaces,
		"team":         team,
		"created_at":   cust.CreatedAt.Format(time.RFC3339),
	}
	if sub != nil {
		resp["subscription"] = map[string]any{
			"id":          sub.ID,
			"tier":        sub.Tier,
			"status":      sub.Status,
			"is_comp":     sub.IsComp(),
			"comp_reason": sub.CompReason,
			"starts_at":   sub.StartsAt.Format(time.RFC3339),
		}
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleAdminSetComp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Comp   bool   `json:"comp"`
		Tier   string `json:"tier"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	cust, err := s.store.GetOrgByName(name)
	if err != nil {
		http.Error(w, "customer not found", http.StatusNotFound)
		return
	}
	if req.Comp {
		if req.Tier == "" {
			req.Tier = "bolt"
		}
		if req.Reason == "" {
			req.Reason = "admin-comp"
		}
		if err := s.store.GrantCompSubscription(cust.ID, req.Tier, req.Reason); err != nil {
			log.Printf("admin: grant comp for %s: %v", name, err)
			http.Error(w, "failed to grant comp", http.StatusInternalServerError)
			return
		}
	} else {
		if err := s.store.DB().WithContext(store.WithOrg(context.Background(), cust.ID)).Model(&store.Subscription{}).
			Where("status = 'active' AND comp_reason != ''").
			Updates(map[string]any{
				"status":     "cancelled",
				"updated_at": time.Now(),
			}).Error; err != nil {
			log.Printf("admin: revoke comp for %s: %v", name, err)
			http.Error(w, "failed to revoke comp", http.StatusInternalServerError)
			return
		}
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleAdminSetTier(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Tier string `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Tier == "" {
		http.Error(w, "tier required", http.StatusBadRequest)
		return
	}
	cust, err := s.store.GetOrgByName(name)
	if err != nil {
		http.Error(w, "customer not found", http.StatusNotFound)
		return
	}
	if err := s.store.GrantCompSubscription(cust.ID, req.Tier, "admin-set-tier"); err != nil {
		log.Printf("admin: set tier for %s: %v", name, err)
		http.Error(w, "failed to set tier", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// ── Bucket handlers ─────────────────────────────────────────────────

func (s *Server) handleAdminListBuckets(w http.ResponseWriter, r *http.Request) {
	buckets, err := s.store.ListBuckets()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(buckets)
}

func (s *Server) handleAdminGetBucket(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	b, found, err := s.store.GetBucket(id)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(b)
}

func (s *Server) handleAdminUpdateBucket(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	b, found, err := s.store.GetBucket(id)
	if err != nil || !found {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var req struct {
		WindowDuration       *string `json:"window_duration"`
		TargetIngestingCount *int    `json:"target_ingesting_count"`
		Tier                 *string `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.WindowDuration != nil {
		b.WindowDuration = *req.WindowDuration
	}
	if req.TargetIngestingCount != nil {
		b.TargetIngestingCount = *req.TargetIngestingCount
	}
	if req.Tier != nil {
		b.Tier = *req.Tier
	}
	if err := s.store.SaveBucket(&b); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(b)
}

// ── Shard handlers ──────────────────────────────────────────────────
//
// The API server is a thin forwarder for shard lifecycle: it never reads
// shard state or validates transitions itself (that would drag the
// duckdb-linked logdb package into this cgo-free binary). The coordinator
// hosts the shard FSMs, owns the shard_records table, and enforces the
// state/retention preconditions; here we just relay the call and translate
// its gRPC status into an HTTP code.

// adminShardView is the JSON shape the admin UI's AdminShard type consumes.
// Built from pb.ShardInfo so the response carries only display metadata —
// notably not the per-shard HeartbeatToken the full row used to leak.
type adminShardView struct {
	ID          string
	WindowStart string
	WindowEnd   string
	State       string
	Addr        string
	InstanceID  string
	ParquetPath string
	Tier        string
	RowCount    int64
	CreatedAt   string
	BucketID    int64
}

// writeShardOpError maps the coordinator's gRPC status to an HTTP response:
// NotFound→404, FailedPrecondition→409 (wrong state or within retention),
// everything else→500. The status message is forwarded verbatim so the
// admin UI's substring checks (e.g. "retention") keep working.
func writeShardOpError(w http.ResponseWriter, action, shardID string, err error) {
	st, _ := status.FromError(err)
	switch st.Code() {
	case codes.NotFound:
		http.Error(w, st.Message(), http.StatusNotFound)
	case codes.FailedPrecondition:
		http.Error(w, st.Message(), http.StatusConflict)
	default:
		log.Printf("admin: %s shard %s: %v", action, shardID, err)
		http.Error(w, action+" failed: "+st.Message(), http.StatusInternalServerError)
	}
}

func (s *Server) handleAdminListShards(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	resp, err := s.coordClient.ListShards(s.coordCtx(r), &pb.ListShardsRequest{BucketId: id})
	if err != nil {
		log.Printf("admin: list shards for bucket %d: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]adminShardView, 0, len(resp.Shards))
	for _, sh := range resp.Shards {
		out = append(out, adminShardView{
			ID:          sh.Id,
			WindowStart: sh.WindowStart,
			WindowEnd:   sh.WindowEnd,
			State:       sh.State,
			Addr:        sh.Addr,
			InstanceID:  sh.InstanceId,
			ParquetPath: sh.ParquetPath,
			Tier:        sh.Tier,
			RowCount:    sh.RowCount,
			CreatedAt:   sh.CreatedAt,
			BucketID:    sh.BucketId,
		})
	}
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleAdminArchiveShard(w http.ResponseWriter, r *http.Request) {
	shardID := r.PathValue("id")
	if shardID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	_, err := s.coordClient.ForceArchiveShard(s.coordCtx(r), &pb.ForceArchiveShardRequest{
		ShardId: shardID,
	})
	if err != nil {
		writeShardOpError(w, "archive", shardID, err)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "archived"})
}

func (s *Server) handleAdminDeleteShard(w http.ResponseWriter, r *http.Request) {
	shardID := r.PathValue("id")
	if shardID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	var req struct {
		Force bool `json:"force"`
	}
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req)
	}
	_, err := s.coordClient.DeleteArchivedShard(s.coordCtx(r), &pb.DeleteArchivedShardRequest{
		ShardId: shardID,
		Force:   req.Force,
	})
	if err != nil {
		writeShardOpError(w, "delete", shardID, err)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}
