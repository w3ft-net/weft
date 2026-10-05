package service

import (
	"context"
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
)

// The cron-mode admin RPCs drive docs/plans/cron-claims-rollout.md: which
// crons run under per-slot claims instead of per-cron leases.

func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (s *Server) ListCronModes(_ context.Context, _ *pb.ListCronModesRequest) (*pb.ListCronModesResponse, error) {
	if s.Store == nil {
		return nil, status.Errorf(codes.Unavailable, "store not configured")
	}
	modes, err := s.Store.CronModes()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list cron modes: %v", err)
	}
	out := make([]*pb.CronModeInfo, 0, len(modes))
	for _, m := range modes {
		out = append(out, &pb.CronModeInfo{
			WatchId:     uint64(m.WatchID),
			OrgId:       int64(m.OrgID),
			Name:        m.Name,
			Schedule:    m.Schedule,
			Mode:        m.Mode,
			LeaseHolder: m.Holder,
			AcquiredAt:  rfc3339OrEmpty(m.AcquiredAt),
			ExpiresAt:   rfc3339OrEmpty(m.ExpiresAt),
		})
	}
	return &pb.ListCronModesResponse{Crons: out}, nil
}

func (s *Server) EnableCronClaims(_ context.Context, req *pb.EnableCronClaimsRequest) (*pb.EnableCronClaimsResponse, error) {
	if s.Store == nil {
		return nil, status.Errorf(codes.Unavailable, "store not configured")
	}
	ids := dedupeIDs(req.WatchIds)
	if len(ids) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "watch_ids is required")
	}
	modes, err := s.Store.CronModes()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list cron modes: %v", err)
	}
	byID := make(map[uint]store.CronMode, len(modes))
	for _, m := range modes {
		byID[m.WatchID] = m
	}
	// Validate everything first: a typo'd id would otherwise leave a stray
	// tombstone row for a cron that doesn't exist, and a partial apply is
	// harder to reason about mid-rollout than a clean refusal.
	var unknown []uint
	for _, id := range ids {
		if _, ok := byID[id]; !ok {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return nil, status.Errorf(codes.NotFound, "no enabled cron with id(s) %v", unknown)
	}

	resp := &pb.EnableCronClaimsResponse{}
	for _, id := range ids {
		if byID[id].Mode == store.CronModeClaims {
			resp.AlreadyClaims = append(resp.AlreadyClaims, uint64(id))
			continue
		}
		if err := s.Store.TombstoneCronLease(id); err != nil {
			return resp, status.Errorf(codes.Internal, "tombstone cron %d (applied so far: %v): %v", id, resp.Changed, err)
		}
		resp.Changed = append(resp.Changed, uint64(id))
	}
	return resp, nil
}

func (s *Server) RollbackCronClaims(_ context.Context, req *pb.RollbackCronClaimsRequest) (*pb.RollbackCronClaimsResponse, error) {
	if s.Store == nil {
		return nil, status.Errorf(codes.Unavailable, "store not configured")
	}
	if req.All == (len(req.WatchIds) > 0) {
		return nil, status.Errorf(codes.InvalidArgument, "give either watch_ids or all, not both or neither")
	}
	ids := dedupeIDs(req.WatchIds)
	if req.All {
		tombs, err := s.Store.CronTombstones()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list tombstones: %v", err)
		}
		for id := range tombs {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}

	resp := &pb.RollbackCronClaimsResponse{WindowSeconds: int32(store.CronRollbackTTL / time.Second)}
	for _, id := range ids {
		ok, err := s.Store.RollbackCronTombstone(id)
		if err != nil {
			return resp, status.Errorf(codes.Internal, "rollback cron %d (applied so far: %v): %v", id, resp.RolledBack, err)
		}
		if ok {
			resp.RolledBack = append(resp.RolledBack, uint64(id))
		} else {
			resp.NotClaims = append(resp.NotClaims, uint64(id))
		}
	}
	return resp, nil
}

// dedupeIDs returns the distinct ids in ascending order.
func dedupeIDs(in []uint64) []uint {
	seen := make(map[uint]bool, len(in))
	out := make([]uint, 0, len(in))
	for _, v := range in {
		id := uint(v)
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
