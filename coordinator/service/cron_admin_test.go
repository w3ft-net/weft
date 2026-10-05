package service

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
)

func cronAdminFixture(t *testing.T) (*Server, []uint) {
	t.Helper()
	db, cleanup := testStore(t)
	t.Cleanup(cleanup)
	org, err := db.FindOrCreateOrg("cron-admin-test")
	if err != nil {
		t.Fatal(err)
	}
	var ids []uint
	adb := db.DB().WithContext(store.WithoutTenantScope(context.Background()))
	for _, name := range []string{"a", "b", "c"} {
		w := store.CronWatch{OrgID: org.ID, Name: "admin-" + name, Schedule: "every 5m", Pipeline: "true", Enabled: true}
		if err := adb.Create(&w).Error; err != nil {
			t.Fatal(err)
		}
		ids = append(ids, w.ID)
	}
	return &Server{Store: db}, ids
}

func modeOf(t *testing.T, s *Server, id uint) string {
	t.Helper()
	resp, err := s.ListCronModes(context.Background(), &pb.ListCronModesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Crons {
		if c.WatchId == uint64(id) {
			return c.Mode
		}
	}
	t.Fatalf("cron %d not listed", id)
	return ""
}

func TestEnableCronClaims_TombstonesNamedCronsOnly(t *testing.T) {
	s, ids := cronAdminFixture(t)
	resp, err := s.EnableCronClaims(context.Background(), &pb.EnableCronClaimsRequest{WatchIds: []uint64{uint64(ids[0]), uint64(ids[0])}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resp.Changed, []uint64{uint64(ids[0])}) {
		t.Fatalf("changed = %v, want just %d (deduped)", resp.Changed, ids[0])
	}
	if got := modeOf(t, s, ids[0]); got != store.CronModeClaims {
		t.Errorf("target mode = %q", got)
	}
	if got := modeOf(t, s, ids[1]); got != store.CronModeLease {
		t.Errorf("untouched cron mode = %q", got)
	}

	// Idempotent: already-claims is reported, not re-applied.
	resp, err = s.EnableCronClaims(context.Background(), &pb.EnableCronClaimsRequest{WatchIds: []uint64{uint64(ids[0])}})
	if err != nil || len(resp.Changed) != 0 || len(resp.AlreadyClaims) != 1 {
		t.Fatalf("second enable: resp=%v err=%v", resp, err)
	}
}

func TestEnableCronClaims_UnknownIDRefusesWholeCall(t *testing.T) {
	s, ids := cronAdminFixture(t)
	_, err := s.EnableCronClaims(context.Background(), &pb.EnableCronClaimsRequest{WatchIds: []uint64{uint64(ids[0]), 987654321}})
	wantCode(t, err, codes.NotFound)
	if got := modeOf(t, s, ids[0]); got != store.CronModeLease {
		t.Errorf("valid id was applied despite a refused call: mode %q", got)
	}
}

func TestEnableCronClaims_RequiresIDs(t *testing.T) {
	s, _ := cronAdminFixture(t)
	_, err := s.EnableCronClaims(context.Background(), &pb.EnableCronClaimsRequest{})
	wantCode(t, err, codes.InvalidArgument)
}

func TestRollbackCronClaims(t *testing.T) {
	s, ids := cronAdminFixture(t)
	ctx := context.Background()
	if _, err := s.EnableCronClaims(ctx, &pb.EnableCronClaimsRequest{WatchIds: []uint64{uint64(ids[0]), uint64(ids[1])}}); err != nil {
		t.Fatal(err)
	}

	// By id: one tombstoned, one not.
	resp, err := s.RollbackCronClaims(ctx, &pb.RollbackCronClaimsRequest{WatchIds: []uint64{uint64(ids[0]), uint64(ids[2])}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resp.RolledBack, []uint64{uint64(ids[0])}) || !slices.Equal(resp.NotClaims, []uint64{uint64(ids[2])}) {
		t.Fatalf("rolled_back=%v not_claims=%v", resp.RolledBack, resp.NotClaims)
	}
	if resp.WindowSeconds != 45 {
		t.Errorf("window_seconds = %d, want 45", resp.WindowSeconds)
	}
	if got := modeOf(t, s, ids[0]); got != store.CronModeRollback {
		t.Errorf("rolled-back cron mode = %q", got)
	}

	// All: picks up the remaining tombstone.
	resp, err = s.RollbackCronClaims(ctx, &pb.RollbackCronClaimsRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(resp.RolledBack, uint64(ids[1])) {
		t.Fatalf("all did not roll back %d: %v", ids[1], resp.RolledBack)
	}
	if got := modeOf(t, s, ids[1]); got != store.CronModeRollback {
		t.Errorf("mode after rollback all = %q", got)
	}
}

func TestRollbackCronClaims_NeedsIDsXorAll(t *testing.T) {
	s, ids := cronAdminFixture(t)
	for name, req := range map[string]*pb.RollbackCronClaimsRequest{
		"neither": {},
		"both":    {All: true, WatchIds: []uint64{uint64(ids[0])}},
	} {
		_, err := s.RollbackCronClaims(context.Background(), req)
		if err == nil {
			t.Errorf("%s: expected InvalidArgument, got nil", name)
			continue
		}
		wantCode(t, err, codes.InvalidArgument)
	}
}

// The new RPCs sit behind the same shared-token wall as the shard ones.
func TestCronAdminRPCsRequireAdminToken(t *testing.T) {
	s, _ := cronAdminFixture(t)
	target := startTestAdminServer(t, s, "tok")
	client, closeFn := newAdminClient(t, target)
	defer closeFn()
	_, err := client.ListCronModes(ctxWithBearer(""), &pb.ListCronModesRequest{})
	wantCode(t, err, codes.Unauthenticated)
	if _, err := client.ListCronModes(ctxWithBearer("tok"), &pb.ListCronModesRequest{}); err != nil {
		t.Fatalf("with token: %v", err)
	}
}
