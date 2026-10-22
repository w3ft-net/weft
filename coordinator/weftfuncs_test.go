package coordinator

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// saveCronResult persists a cron result via the exported store
// surface. It mirrors the logic that formerly lived in
// store.SaveCronResult, which was relocated into production callers.
func saveCronResult(db *store.Store, result *store.CronResult) error {
	if result.Value == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(result.Stdout), 64); err == nil {
			result.Value = &v
		}
	}
	return db.DB().Create(result).Error
}

func testStoreAndCustomer(t *testing.T) (*store.Store, store.OrgID) {
	t.Helper()
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}

	tx, rollback := db.BeginTest()
	t.Cleanup(func() {
		rollback()
		db.Close()
	})

	cust, err := tx.FindOrCreateOrg("weftfuncs-test")
	if err != nil {
		t.Fatal(err)
	}
	return tx, cust.ID
}

// testQueryStore prepares a tx-scoped Store satisfying the wiring
// Coordinator.Query expects: a customer of the given name, the listed
// namespaces under it, and a bolt-tier subscription so the planner's
// retention clamp doesn't trim test fixtures whose timestamps are
// older than trial retention. Used by shard-fan-out tests that exercise
// the whole Query path against in-memory shards.
func testQueryStore(t testing.TB, customer string, namespaces ...string) *store.Store {
	return testQueryStoreOpts(t, customer, false, namespaces...)
}

// testQueryStoreReal returns a real-DB Store (NOT BeginTest-wrapped).
// Use this when the test runs concurrent queries against the store —
// BeginTest binds a single *sql.Tx to one connection, which pgx
// doesn't allow goroutines to share. The real store goes through
// gorm's connection pool, so each goroutine gets its own. Cleanup
// truncates the touched tables.
func testQueryStoreReal(t testing.TB, customer string, namespaces ...string) *store.Store {
	return testQueryStoreOpts(t, customer, true, namespaces...)
}

func testQueryStoreOpts(t testing.TB, customer string, realDB bool, namespaces ...string) *store.Store {
	t.Helper()
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}

	target := db
	if !realDB {
		tx, rollback := db.BeginTest()
		t.Cleanup(func() {
			rollback()
			db.Close()
		})
		target = tx
	} else {
		// Scoped cleanup: delete only the rows this test inserts, not
		// every row in the shared weft DB. A blanket TruncateTestTables
		// races other packages' tests (api/*, store/team_perms) that
		// run in parallel against the same database.
		t.Cleanup(func() {
			deleteOrgData(db, customer)
			db.Close()
		})
		// Pre-clean too, in case a previous crashed run left this
		// customer behind.
		deleteOrgData(db, customer)
	}

	cust := store.TestOrg(t, target, customer)
	for _, ns := range namespaces {
		if _, err := target.FindOrCreateNamespace(cust.ID, ns); err != nil {
			t.Fatalf("create namespace %s: %v", ns, err)
		}
	}
	if err := target.CreateSubscription(&store.Subscription{
		ID:       "test-bolt-" + customer,
		OrgID:    cust.ID,
		Tier:     string(store.TierBolt),
		Status:   "active",
		StartsAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed bolt subscription: %v", err)
	}
	return target
}

// deleteOrgData removes every row scoped to a single customer
// from the shared weft DB, leaving other test packages' data alone.
// All customer-scoped tables (subscriptions included, as of migration
// 053) key on the numeric customer id, resolved here from the name.
func deleteOrgData(db *store.Store, customer string) {
	_ = db.ExecForTest("DELETE FROM subscriptions WHERE org_id IN (SELECT id FROM organizations WHERE name = ?)", customer)
	_ = db.ExecForTest("DELETE FROM namespaces WHERE org_id IN (SELECT id FROM organizations WHERE name = ?)", customer)
	_ = db.ExecForTest("DELETE FROM organizations WHERE name = ?", customer)
}

func seedWatch(t *testing.T, db *store.Store, custID store.OrgID, name string, values []string, interval time.Duration) {
	t.Helper()
	err := db.SaveCronWatch(custID, "", name, "every 1m", "echo test", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := db.GetCronWatch(custID, name)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, v := range values {
		err := saveCronResult(db, &store.CronResult{
			CronWatchID: w.ID,
			RanAt:       now.Add(-time.Duration(len(values)-1-i) * interval),
			Stdout:      v,
			ExitCode:    0,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestWeftFuncs_Latest(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "latest-test", []string{"10", "20", "30"}, time.Minute)

	wf := &WeftFuncs{Store: db, OrgID: custID}
	val, err := wf.Latest("latest-test")
	if err != nil {
		t.Fatal(err)
	}
	if val != "30" {
		t.Errorf("expected 30, got %q", val)
	}
}

func TestWeftFuncs_Latest_NotFound(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	wf := &WeftFuncs{Store: db, OrgID: custID}
	_, err := wf.Latest("nonexistent-watch")
	if err == nil {
		t.Error("expected error for nonexistent watch")
	}
}

func TestWeftFuncs_Avg(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "avg-test", []string{"10", "20", "30"}, time.Minute)

	wf := &WeftFuncs{Store: db, OrgID: custID}
	val, err := wf.Avg("avg-test", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if val != 20.0 {
		t.Errorf("expected 20, got %f", val)
	}
}

func TestWeftFuncs_Max(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "max-test", []string{"5", "42", "17"}, time.Minute)

	wf := &WeftFuncs{Store: db, OrgID: custID}
	val, err := wf.Max("max-test", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if val != 42.0 {
		t.Errorf("expected 42, got %f", val)
	}
}

func TestWeftFuncs_Min(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "min-test", []string{"5", "42", "17"}, time.Minute)

	wf := &WeftFuncs{Store: db, OrgID: custID}
	val, err := wf.Min("min-test", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if val != 5.0 {
		t.Errorf("expected 5, got %f", val)
	}
}

func TestWeftFuncs_Sum(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "sum-test", []string{"10", "20", "30"}, time.Minute)

	wf := &WeftFuncs{Store: db, OrgID: custID}
	val, err := wf.Sum("sum-test", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if val != 60.0 {
		t.Errorf("expected 60, got %f", val)
	}
}

func TestWeftFuncs_Stddev(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	// Values: 10, 20, 30 → mean=20, variance=((100+0+100)/3)=66.67, stddev≈8.165
	seedWatch(t, db, custID, "stddev-test", []string{"10", "20", "30"}, time.Minute)

	wf := &WeftFuncs{Store: db, OrgID: custID}
	val, err := wf.Stddev("stddev-test", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expected := math.Sqrt(200.0 / 3.0) // ≈8.165
	if math.Abs(val-expected) > 0.01 {
		t.Errorf("expected ~%.3f, got %.3f", expected, val)
	}
}

func TestWeftFuncs_Stddev_NeedsTwoResults(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "stddev-one", []string{"42"}, time.Minute)

	wf := &WeftFuncs{Store: db, OrgID: custID}
	_, err := wf.Stddev("stddev-one", 1*time.Hour)
	if err == nil {
		t.Error("expected error for single result")
	}
}

func TestWeftFuncs_SkipsNonNumeric(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "mixed-test", []string{"10", "not-a-number", "30", "ERROR", "20"}, time.Minute)

	wf := &WeftFuncs{Store: db, OrgID: custID}

	avg, err := wf.Avg("mixed-test", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if avg != 20.0 { // (10+30+20)/3
		t.Errorf("avg: expected 20, got %f", avg)
	}

	max, _ := wf.Max("mixed-test", 1*time.Hour)
	if max != 30.0 {
		t.Errorf("max: expected 30, got %f", max)
	}

	min, _ := wf.Min("mixed-test", 1*time.Hour)
	if min != 10.0 {
		t.Errorf("min: expected 10, got %f", min)
	}

	sum, _ := wf.Sum("mixed-test", 1*time.Hour)
	if sum != 60.0 {
		t.Errorf("sum: expected 60, got %f", sum)
	}
}

func TestWeftFuncs_DurationCap(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	wf := &WeftFuncs{Store: db, OrgID: custID}

	_, err := wf.Avg("any-watch", 25*time.Hour)
	if err == nil {
		t.Error("expected error for duration > 24h")
	}

	_, err = wf.Avg("any-watch", -1*time.Hour)
	if err == nil {
		t.Error("expected error for negative duration")
	}
}

func TestWeftFuncs_TimeRangeFiltering(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	// Seed with values spread over 2 hours. Query only last 30m.
	err := db.SaveCronWatch(custID, "", "range-filter-test", "every 1m", "echo test", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := db.GetCronWatch(custID, "range-filter-test")
	now := time.Now()

	// Old values (2h ago) — should be excluded from 30m query.
	for _, v := range []string{"100", "200"} {
		saveCronResult(db, &store.CronResult{
			CronWatchID: w.ID,
			RanAt:       now.Add(-2 * time.Hour),
			Stdout:      v,
		})
	}
	// Recent values (5m ago) — should be included.
	for _, v := range []string{"10", "20"} {
		saveCronResult(db, &store.CronResult{
			CronWatchID: w.ID,
			RanAt:       now.Add(-5 * time.Minute),
			Stdout:      v,
		})
	}

	wf := &WeftFuncs{Store: db, OrgID: custID}
	avg, err := wf.Avg("range-filter-test", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if avg != 15.0 { // (10+20)/2, not including old 100/200
		t.Errorf("expected 15 (recent only), got %f", avg)
	}
}

func TestWeftFuncs_FloatValues(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "float-test", []string{"1.5", "2.5", "3.0"}, time.Minute)

	wf := &WeftFuncs{Store: db, OrgID: custID}

	avg, _ := wf.Avg("float-test", 1*time.Hour)
	expected := (1.5 + 2.5 + 3.0) / 3.0
	if math.Abs(avg-expected) > 0.001 {
		t.Errorf("avg: expected %.3f, got %.3f", expected, avg)
	}

	sum, _ := wf.Sum("float-test", 1*time.Hour)
	if math.Abs(sum-7.0) > 0.001 {
		t.Errorf("sum: expected 7.0, got %.3f", sum)
	}
}

func TestWeftFuncs_EmptyResults(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	// Create watch with no results.
	db.SaveCronWatch(custID, "", "empty-test", "every 1m", "echo test", true, nil)

	wf := &WeftFuncs{Store: db, OrgID: custID}

	_, err := wf.Avg("empty-test", 1*time.Hour)
	if err == nil {
		t.Error("expected error for empty results")
	}

	_, err = wf.Max("empty-test", 1*time.Hour)
	if err == nil {
		t.Error("expected error for empty results")
	}
}

func TestParseAggDuration(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
		err   bool
	}{
		{"30m", 30 * time.Minute, false},
		{"1h", time.Hour, false},
		{"6h", 6 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"25h", 25 * time.Hour, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"401d", 0, true}, // exceeds cap
		{"-1h", 0, true},  // negative
		{"abc", 0, true},  // invalid
		{"0s", 0, true},   // zero
	}
	for _, tt := range tests {
		d, err := ParseAggDuration(tt.input)
		if tt.err && err == nil {
			t.Errorf("ParseAggDuration(%q): expected error", tt.input)
		}
		if !tt.err && err != nil {
			t.Errorf("ParseAggDuration(%q): unexpected error: %v", tt.input, err)
		}
		if !tt.err && d != tt.want {
			t.Errorf("ParseAggDuration(%q): got %v, want %v", tt.input, d, tt.want)
		}
	}
}

func TestParseRawDuration_CapsAtRawRetention(t *testing.T) {
	if d, err := ParseRawDuration("48h"); err != nil || d != 48*time.Hour {
		t.Errorf("ParseRawDuration(48h) = %v, %v", d, err)
	}
	// 192h = 8d, just over the 7d of raw results.
	if _, err := ParseRawDuration("192h"); err == nil {
		t.Error("ParseRawDuration(8d): expected reject, got nil")
	}
}

func TestWeftFuncs_First(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	// seedWatch lays values left-to-right with the LAST element at now.
	// So "10" is oldest, "30" is newest.
	seedWatch(t, db, custID, "first-test", []string{"10", "20", "30"}, time.Minute)

	wf := &WeftFuncs{Store: db, OrgID: custID}
	got, err := wf.First("first-test", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got != 10.0 {
		t.Errorf("First: expected 10, got %f", got)
	}
}

func TestWeftFuncs_First_AcceptsLongerWindow(t *testing.T) {
	// First should accept a window > 24h, up to the raw retention.
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "first-long", []string{"5"}, time.Minute)
	wf := &WeftFuncs{Store: db, OrgID: custID}
	if _, err := wf.First("first-long", 48*time.Hour); err != nil {
		t.Errorf("First with 48h window: unexpected error: %v", err)
	}
	if _, err := wf.First("first-long", 8*24*time.Hour); err == nil {
		t.Error("First with 8d window: expected cap error, got nil")
	}
}

func TestWeftFuncs_Count(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	// Mix numeric and non-numeric — Count counts numeric results only, since
	// the rollups that serve long windows don't keep the others.
	seedWatch(t, db, custID, "count-test", []string{"10", "ERROR", "30", "20"}, time.Minute)
	wf := &WeftFuncs{Store: db, OrgID: custID}
	got, err := wf.Count("count-test", 1*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3.0 {
		t.Errorf("Count: expected 3, got %f", got)
	}
}

func TestWeftFuncs_Percentile(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "pct-test", []string{"10", "20", "30", "40", "50"}, time.Minute)
	wf := &WeftFuncs{Store: db, OrgID: custID}

	cases := []struct {
		p    float64
		want float64
	}{
		{0, 10},   // min
		{50, 30},  // median (middle of 5)
		{100, 50}, // max
		{25, 20},  // exact rank 1
		{75, 40},  // exact rank 3
	}
	for _, c := range cases {
		got, err := wf.Percentile("pct-test", 1*time.Hour, c.p)
		if err != nil {
			t.Fatalf("p=%v: %v", c.p, err)
		}
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("p=%v: got %f, want %f", c.p, got, c.want)
		}
	}
}

func TestWeftFuncs_Percentile_LinearInterpolation(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	// 4 values: ranks 0..3. p=33.33 → rank 1.0 = value at index 1.
	// p=50 → rank 1.5 = midpoint between index 1 and index 2.
	seedWatch(t, db, custID, "pct-interp", []string{"10", "20", "30", "40"}, time.Minute)
	wf := &WeftFuncs{Store: db, OrgID: custID}
	got, err := wf.Percentile("pct-interp", 1*time.Hour, 50)
	if err != nil {
		t.Fatal(err)
	}
	// rank = 0.5 * 3 = 1.5 → vals[1] + 0.5*(vals[2]-vals[1]) = 20 + 5 = 25.
	if math.Abs(got-25.0) > 1e-9 {
		t.Errorf("p=50 of [10,20,30,40]: got %f, want 25", got)
	}
}

func TestWeftFuncs_Percentile_OutOfRange(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "pct-bounds", []string{"1", "2", "3"}, time.Minute)
	wf := &WeftFuncs{Store: db, OrgID: custID}
	if _, err := wf.Percentile("pct-bounds", 1*time.Hour, -1); err == nil {
		t.Error("expected error for p < 0")
	}
	if _, err := wf.Percentile("pct-bounds", 1*time.Hour, 101); err == nil {
		t.Error("expected error for p > 100")
	}
}

func TestWeftFuncs_Delta(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	// Oldest=10, newest=50.
	seedWatch(t, db, custID, "delta-test", []string{"10", "20", "30", "40", "50"}, time.Minute)
	wf := &WeftFuncs{Store: db, OrgID: custID}
	got, err := wf.Delta("delta-test", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got != 40.0 { // 50 - 10
		t.Errorf("Delta: expected 40, got %f", got)
	}
}

func TestWeftFuncs_Delta_NeedsTwoResults(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "delta-one", []string{"42"}, time.Minute)
	wf := &WeftFuncs{Store: db, OrgID: custID}
	if _, err := wf.Delta("delta-one", 1*time.Hour); err == nil {
		t.Error("expected error for single result")
	}
}

func TestWeftFuncs_Slope(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	// Delta = 50 - 10 = 40, over a window of 600s → slope = 40/600.
	seedWatch(t, db, custID, "slope-test", []string{"10", "20", "30", "40", "50"}, time.Minute)
	wf := &WeftFuncs{Store: db, OrgID: custID}
	got, err := wf.Slope("slope-test", 600*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := 40.0 / 600.0
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("Slope: got %f, want %f", got, want)
	}
}

func TestWeftFuncs_Rate(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	// 3 values over 2 minutes wall time, 10-minute window so all fall in.
	// Sum = 10+20+30 = 60; window = 600s → rate = 0.1/sec.
	seedWatch(t, db, custID, "rate-test", []string{"10", "20", "30"}, time.Minute)
	wf := &WeftFuncs{Store: db, OrgID: custID}
	got, err := wf.Rate("rate-test", 600*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := 0.1
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("Rate: got %f, want %f", got, want)
	}
}
