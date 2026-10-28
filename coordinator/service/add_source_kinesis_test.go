package service

import (
	"context"
	"testing"

	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/coordinator"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
)

// TestAddSource_Kinesis covers AddSource's only remaining flow (poll-mode
// CloudWatch was retired): no cross-account role required, no schema at
// creation time, and the resulting row must be StatusPending/SourceTypeKinesis.
func TestAddSource_Kinesis(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()

	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}, Store: st})
	srv := &Server{Coord: coord, Store: st}

	cust, err := st.FindOrCreateOrg("kinesis-addsource-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.FindOrCreateNamespace(cust.ID, "ops"); err != nil {
		t.Fatal(err)
	}

	ctx := auth.WithSession(context.Background(), &auth.Session{
		ActiveOrgID:        cust.ID,
		ActiveCustomerName: cust.Name,
		EmailVerified:      true,
		IsOwner:            true,
	})

	if _, err := srv.AddSource(ctx, &pb.AddSourceRequest{
		Namespace: "ops",
		Service:   "compactor",
		LogGroup:  "/aws/lambda/weft-compactor",
		Region:    "us-east-2",
	}); err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	lg, err := st.FindLogGroup(cust.ID, "ops", "compactor")
	if err != nil {
		t.Fatal(err)
	}
	if lg == nil {
		t.Fatal("expected a log group row after AddSource, got nil")
	}
	if lg.SourceType != store.SourceTypeKinesis {
		t.Errorf("SourceType = %q, want %q", lg.SourceType, store.SourceTypeKinesis)
	}
	if lg.Status != store.StatusPending {
		t.Errorf("Status = %q, want %q — no schema submitted, nothing to configure yet", lg.Status, store.StatusPending)
	}
	if lg.LogGroupName != "/aws/lambda/weft-compactor" {
		t.Errorf("LogGroupName = %q, want /aws/lambda/weft-compactor", lg.LogGroupName)
	}
}

// TestAddSource_Kinesis_RequiresLogGroup ensures the kinesis branch
// still validates its one required field even though it skips role_arn
// validation entirely.
func TestAddSource_Kinesis_RequiresLogGroup(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()

	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}, Store: st})
	srv := &Server{Coord: coord, Store: st}

	cust, err := st.FindOrCreateOrg("kinesis-addsource-validate-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.FindOrCreateNamespace(cust.ID, "ops"); err != nil {
		t.Fatal(err)
	}

	ctx := auth.WithSession(context.Background(), &auth.Session{
		ActiveOrgID:        cust.ID,
		ActiveCustomerName: cust.Name,
		EmailVerified:      true,
		IsOwner:            true,
	})

	_, err = srv.AddSource(ctx, &pb.AddSourceRequest{
		Namespace: "ops",
		Service:   "compactor",
	})
	if err == nil {
		t.Fatal("expected an error for missing log_group, got nil")
	}
}
