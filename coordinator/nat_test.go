//go:build !defaultshape

package coordinator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/graybeardcorp/weft/store"
)

// resetNATLease clears any "nat-holder" row left behind by an earlier
// test. Tests in this file share one pooled DB per process
// (store.TestDSN's doc comment), and they all exercise the same
// production lease name by design (it's a package constant, not
// parameterized) — so each test must start from a clean slate rather
// than assume it's the first writer.
func resetNATLease(t *testing.T, db *store.Store) {
	t.Helper()
	if err := db.DB().Exec("DELETE FROM leases WHERE name = ?", natLeaseName).Error; err != nil {
		t.Fatalf("reset nat-holder lease: %v", err)
	}
}

// fakeNATEC2 records ReplaceRoute/CreateRoute calls instead of hitting AWS.
// replaceErr lets a test force the ReplaceRoute→CreateRoute fallback path.
type fakeNATEC2 struct {
	mu          sync.Mutex
	replaceErr  error
	replaceCall int
	createCall  int
	lastENI     string
}

func (f *fakeNATEC2) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return nil, errors.New("not used in these tests")
}

func (f *fakeNATEC2) ReplaceRoute(ctx context.Context, in *ec2.ReplaceRouteInput, opts ...func(*ec2.Options)) (*ec2.ReplaceRouteOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replaceCall++
	if f.replaceErr != nil {
		return nil, f.replaceErr
	}
	f.lastENI = aws.ToString(in.NetworkInterfaceId)
	return &ec2.ReplaceRouteOutput{}, nil
}

func (f *fakeNATEC2) CreateRoute(ctx context.Context, in *ec2.CreateRouteInput, opts ...func(*ec2.Options)) (*ec2.CreateRouteOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCall++
	f.lastENI = aws.ToString(in.NetworkInterfaceId)
	return &ec2.CreateRouteOutput{}, nil
}

func (f *fakeNATEC2) calls() (replace, create int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.replaceCall, f.createCall
}

// TestNATElector_AcquiresLeaseAndClaimsRouteOnce confirms the core
// contract: winning the lease claims the route exactly once, and
// renewing an already-held lease on later ticks does not re-claim it.
func TestNATElector_AcquiresLeaseAndClaimsRouteOnce(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	resetNATLease(t, db)
	ec2c := &fakeNATEC2{}
	e := NewNATElector(db, ec2c, "rtb-123", "eni-abc", "coord-a")

	var amHolder bool
	e.tick(context.Background(), &amHolder)
	if !amHolder {
		t.Fatal("expected to hold the lease after the first tick")
	}
	replace, create := ec2c.calls()
	if replace != 1 || create != 0 {
		t.Fatalf("expected exactly one ReplaceRoute call, got replace=%d create=%d", replace, create)
	}
	if ec2c.lastENI != "eni-abc" {
		t.Fatalf("claimed route pointed at %q, want eni-abc", ec2c.lastENI)
	}

	// Renewal tick: still holding, must not re-claim the route.
	e.tick(context.Background(), &amHolder)
	replace, create = ec2c.calls()
	if replace != 1 || create != 0 {
		t.Fatalf("renewal tick re-claimed the route: replace=%d create=%d", replace, create)
	}
}

// TestNATElector_FallsBackToCreateRoute confirms a ReplaceRoute failure
// (fresh route table with no existing 0.0.0.0/0 route) falls back to
// CreateRoute, matching the old bash claim's behavior.
func TestNATElector_FallsBackToCreateRoute(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	resetNATLease(t, db)
	ec2c := &fakeNATEC2{replaceErr: errors.New("InvalidRoute.NotFound")}
	e := NewNATElector(db, ec2c, "rtb-123", "eni-abc", "coord-a")

	var amHolder bool
	e.tick(context.Background(), &amHolder)
	if !amHolder {
		t.Fatal("expected to hold the lease even though ReplaceRoute failed and CreateRoute had to run")
	}
	replace, create := ec2c.calls()
	if replace != 1 || create != 1 {
		t.Fatalf("expected ReplaceRoute then CreateRoute fallback, got replace=%d create=%d", replace, create)
	}
}

// TestNATElector_DoesNotChallengeHealthyIncumbent confirms the lease's
// self-renewal bias — the primitive this design leans on instead of
// building separate anti-flap logic (see NATElector's doc comment).
func TestNATElector_DoesNotChallengeHealthyIncumbent(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	resetNATLease(t, db)
	incumbent := &fakeNATEC2{}
	a := NewNATElector(db, incumbent, "rtb-123", "eni-incumbent", "coord-a")
	var aHolds bool
	a.tick(context.Background(), &aHolds)
	if !aHolds {
		t.Fatal("incumbent should win the initially-unheld lease")
	}

	challenger := &fakeNATEC2{}
	b := NewNATElector(db, challenger, "rtb-123", "eni-challenger", "coord-b")
	var bHolds bool
	b.tick(context.Background(), &bHolds)
	if bHolds {
		t.Fatal("challenger must not win the lease while the incumbent is still renewing")
	}
	if replace, create := challenger.calls(); replace != 0 || create != 0 {
		t.Fatalf("challenger must not touch the route without the lease: replace=%d create=%d", replace, create)
	}

	// Incumbent keeps renewing; challenger keeps losing.
	a.tick(context.Background(), &aHolds)
	b.tick(context.Background(), &bHolds)
	if !aHolds || bHolds {
		t.Fatalf("incumbent should keep the lease across ticks: aHolds=%v bHolds=%v", aHolds, bHolds)
	}
}

// TestNATElector_ChallengerTakesOverAfterIncumbentExpiry confirms
// failover actually happens once the incumbent stops renewing —
// the flip side of the anti-flap guarantee above.
func TestNATElector_ChallengerTakesOverAfterIncumbentExpiry(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	resetNATLease(t, db)
	// Seed an already-expired lease directly, simulating an incumbent
	// that died LeaseTTL+ ago rather than waiting out the real TTL here.
	if ok, err := db.AcquireLease(natLeaseName, "coord-dead", -time.Second); err != nil || !ok {
		t.Fatalf("setup: seed expired lease failed: ok=%v err=%v", ok, err)
	}

	challenger := &fakeNATEC2{}
	b := NewNATElector(db, challenger, "rtb-123", "eni-challenger", "coord-b")
	var bHolds bool
	b.tick(context.Background(), &bHolds)
	if !bHolds {
		t.Fatal("challenger should take over an expired lease")
	}
	if replace, _ := challenger.calls(); replace != 1 {
		t.Fatalf("expected challenger to claim the route on takeover, got %d ReplaceRoute calls", replace)
	}
}
