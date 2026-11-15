package coordinator

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"

	"github.com/w3ft-net/weft/tenancy"

	"github.com/w3ft-net/weft/compute"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// fakeClock is a settable clock for the ledger.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newTestLedger(clock *fakeClock, load func(org shape.Added) (int64, error)) (*ColdScanLedger, *atomic.Int32) {
	var loads atomic.Int32
	l := newColdScanLedger(clock.now, func(org shape.Added, _, _ time.Time) (int64, error) {
		loads.Add(1)
		return load(org)
	})
	return l, &loads
}

func TestColdScanLedger_UsedIsTheTableAndWhatThisCoordinatorHasCounted(t *testing.T) {
	clock := &fakeClock{time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	l, loads := newTestLedger(clock, func(shape.Added) (int64, error) { return 1_000, nil })

	l.Record(shapetest.Added(7), 50)
	if got, _ := l.Used(shapetest.Added(7)); got != 1_050 {
		t.Fatalf("used = %d, want the table's 1000 plus 50 pending", got)
	}
	// Flushing moves the 50 into the table; the cached read cannot see it
	// yet, so the ledger must keep counting it.
	entries := l.Drain()
	if len(entries) != 1 || entries[0].Org != shapetest.Added(7) || entries[0].Rows != 50 {
		t.Fatalf("drain = %+v", entries)
	}
	l.Record(shapetest.Added(7), 5)
	if got, _ := l.Used(shapetest.Added(7)); got != 1_055 {
		t.Fatalf("used after flush = %d, want 1055", got)
	}
	if loads.Load() != 1 {
		t.Errorf("the table was read %d times inside the cache window, want 1", loads.Load())
	}

	// Past the TTL the table is read again, and now includes the flush.
	clock.t = clock.t.Add(coldScanCacheTTL + time.Second)
	l.load = func(shape.Added, time.Time, time.Time) (int64, error) { return 1_050, nil }
	if got, _ := l.Used(shapetest.Added(7)); got != 1_055 {
		t.Errorf("used after refresh = %d, want 1055 (1050 flushed + 5 pending)", got)
	}
}

func TestColdScanLedger_OrgsAreSeparateAndMonthsRoll(t *testing.T) {
	clock := &fakeClock{time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)}
	shapetest.NeedsTenant(t)
	l, _ := newTestLedger(clock, func(org shape.Added) (int64, error) {
		if org == shapetest.Added(2) {
			return 20, nil
		}
		return 10, nil
	})
	l.Record(shapetest.Added(1), 3)
	if a, _ := l.Used(shapetest.Added(1)); a != 13 {
		t.Errorf("org 1 = %d, want 13", a)
	}
	if b, _ := l.Used(shapetest.Added(2)); b != 20 {
		t.Errorf("org 2 = %d, want 20 (org 1's scans are not org 2's)", b)
	}
	// A new month starts a new total: last month's pending does not count.
	clock.t = time.Date(2026, 12, 1, 0, 5, 0, 0, time.UTC)
	if a, _ := l.Used(shapetest.Added(1)); a != 10 {
		t.Errorf("org 1 in December = %d, want 10 (November's pending scans excluded)", a)
	}
}

func TestColdScanLedger_RecordIgnoresZeroAndNegative(t *testing.T) {
	l, _ := newTestLedger(&fakeClock{time.Now()}, func(shape.Added) (int64, error) { return 0, nil })
	l.Record(shapetest.Added(1), 0)
	l.Record(shapetest.Added(1), -4)
	if got := l.Drain(); len(got) != 0 {
		t.Errorf("drained %+v", got)
	}
}

func hotDesc() *pb.ShardDescriptor {
	return &pb.ShardDescriptor{ShardId: "hot", GrpcAddr: "10.0.0.1:9000"}
}
func coldDesc() *pb.ShardDescriptor {
	return &pb.ShardDescriptor{ShardId: "cold", LambdaUrl: "https://x", ParquetPath: "s3://b/x.parquet"}
}

func enforcing(used int64, loadErr error) *Coordinator {
	clock := &fakeClock{time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	l, _ := newTestLedger(clock, func(shape.Added) (int64, error) { return used, loadErr })
	return &Coordinator{ColdScans: l}
}

func TestCheckColdScanBudget(t *testing.T) {
	shapetest.NeedsTenant(t)
	cases := []struct {
		name      string
		c         *Coordinator
		allowance int64
		descs     []*pb.ShardDescriptor
		refused   bool
	}{
		{"under the budget", enforcing(testAllowance-1, nil), testAllowance, []*pb.ShardDescriptor{hotDesc(), coldDesc()}, false},
		{"at the budget", enforcing(testAllowance, nil), testAllowance, []*pb.ShardDescriptor{coldDesc()}, true},
		{"over the budget, one cold among hot", enforcing(testAllowance+1, nil), testAllowance, []*pb.ShardDescriptor{hotDesc(), coldDesc()}, true},
		{"over the budget, hot only", enforcing(testAllowance+1, nil), testAllowance, []*pb.ShardDescriptor{hotDesc()}, false},
		{"trial has no cold history", enforcing(0, nil), 0, []*pb.ShardDescriptor{coldDesc()}, true},
		{"unreadable total fails open", enforcing(0, errors.New("db down")), testAllowance, []*pb.ShardDescriptor{coldDesc()}, false},
		{"no ledger", &Coordinator{}, testAllowance, []*pb.ShardDescriptor{coldDesc()}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.checkColdScanBudget(context.Background(), shapetest.Added(1), limitsOf(tc.allowance), tc.descs)
			if tc.refused {
				var be *tenancy.ColdScanBudgetError
				if !errors.As(err, &be) || !errors.Is(err, tenancy.ErrColdScanBudgetExhausted) {
					t.Fatalf("err = %v, want a budget error", err)
				}
				if !be.ResetsAt.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
					t.Errorf("resets at %v, want 1 December", be.ResetsAt)
				}
			} else if err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// fakeStream is a LineStream that knows what it scanned.
type fakeStream struct {
	lines   int
	scanned int64
}

func (f *fakeStream) Next(context.Context) (compute.Line, bool, error) {
	if f.lines == 0 {
		return compute.Line{}, false, nil
	}
	f.lines--
	return compute.Line{Raw: "x"}, true, nil
}
func (f *fakeStream) Close() error       { return nil }
func (f *fakeStream) RowsScanned() int64 { return f.scanned }

// plainStream knows nothing of scans, as a compute from before the count.
type plainStream struct{ fakeStream }

func (plainStream) RowsScanned() {} // shadows the method: not a ScanCounter

type fakeExecutor struct {
	calls  atomic.Int32
	stream compute.LineStream
}

func (f *fakeExecutor) ExecuteQuery(context.Context, *pb.ExecuteQueryRequest) (compute.LineStream, error) {
	f.calls.Add(1)
	return f.stream, nil
}

func drain(t *testing.T, s compute.LineStream) {
	t.Helper()
	for {
		_, ok, err := s.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return
		}
	}
}

func TestExecuteCompute_MetersWhatTheQueryScanned(t *testing.T) {
	shapetest.NeedsTenant(t)
	c := enforcing(0, nil)
	c.Compute = &fakeExecutor{stream: &fakeStream{lines: 2, scanned: 900}}
	s, err := c.executeCompute(context.Background(), shapetest.Added(5), limitsOf(testAllowance), &pb.ExecuteQueryRequest{Shards: []*pb.ShardDescriptor{coldDesc()}})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ColdScans.Drain(); len(got) != 0 {
		t.Fatalf("recorded %+v before the stream ended", got)
	}
	drain(t, s)
	s.Close() // closing after the end must not count twice
	got := c.ColdScans.Drain()
	if len(got) != 1 || got[0].Org != shapetest.Added(5) || got[0].Rows != 900 {
		t.Fatalf("recorded %+v, want org 5 rows 900 once", got)
	}
}

func TestExecuteCompute_CountsAStreamThatIsClosedEarly(t *testing.T) {
	shapetest.NeedsTenant(t)
	c := enforcing(0, nil)
	c.Compute = &fakeExecutor{stream: &fakeStream{lines: 10, scanned: 40}}
	s, _ := c.executeCompute(context.Background(), shapetest.Added(5), limitsOf(testAllowance), &pb.ExecuteQueryRequest{Shards: []*pb.ShardDescriptor{coldDesc()}})
	s.Close()
	if got := c.ColdScans.Drain(); len(got) != 1 || got[0].Rows != 40 {
		t.Fatalf("recorded %+v, want 40", got)
	}
}

func TestExecuteCompute_AStreamWithoutACountChargesNothing(t *testing.T) {
	shapetest.NeedsTenant(t)
	c := enforcing(0, nil)
	c.Compute = &fakeExecutor{stream: &plainStream{fakeStream{lines: 1}}}
	s, _ := c.executeCompute(context.Background(), shapetest.Added(5), limitsOf(testAllowance), &pb.ExecuteQueryRequest{Shards: []*pb.ShardDescriptor{coldDesc()}})
	drain(t, s)
	if got := c.ColdScans.Drain(); len(got) != 0 {
		t.Fatalf("recorded %+v for a stream that cannot count", got)
	}
}

func TestExecuteCompute_RefusalNeverReachesCompute(t *testing.T) {
	shapetest.NeedsTenant(t)
	c := enforcing(testAllowance, nil)
	exec := &fakeExecutor{stream: &fakeStream{}}
	c.Compute = exec
	_, err := c.executeCompute(context.Background(), shapetest.Added(5), limitsOf(testAllowance), &pb.ExecuteQueryRequest{Shards: []*pb.ShardDescriptor{coldDesc()}})
	if !errors.Is(err, tenancy.ErrColdScanBudgetExhausted) || exec.calls.Load() != 0 {
		t.Fatalf("err=%v, compute called %d times; want a refusal before compute", err, exec.calls.Load())
	}
	// The same org's hot-only query still runs.
	if _, err := c.executeCompute(context.Background(), shapetest.Added(5), limitsOf(testAllowance), &pb.ExecuteQueryRequest{Shards: []*pb.ShardDescriptor{hotDesc()}}); err != nil || exec.calls.Load() != 1 {
		t.Fatalf("hot-only query: err=%v calls=%d", err, exec.calls.Load())
	}
}

// testAllowance is a month's cold scan allowance in these tests.
const testAllowance int64 = 1_000_000

// limitsOf is a tenant's limits with the given cold scan allowance.
func limitsOf(allowance int64) tenancy.Limits { return tenancy.Limits{ColdScanLines: &allowance} }
