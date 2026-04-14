package coordinator

import (
	"math"
	"os"
	"testing"
	"time"

	"github.com/kevlarcon/weft/store"
)

func testStoreAndCustomer(t *testing.T) (*store.Store, uint) {
	t.Helper()
	dsn := os.Getenv("WEFT_TEST_DSN")
	if dsn == "" {
		dsn = "host=localhost port=5433 dbname=weft user=weft password=weft sslmode=disable"
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}

	tx, rollback := db.BeginTest()
	t.Cleanup(func() {
		rollback()
		db.Close()
	})

	cust, err := tx.FindOrCreateCustomer("weftfuncs-test")
	if err != nil {
		t.Fatal(err)
	}
	return tx, cust.ID
}

func seedWatch(t *testing.T, db *store.Store, custID uint, name string, values []string, interval time.Duration) {
	t.Helper()
	err := db.SaveCronWatch(custID, name, "every 1m", "echo test", "")
	if err != nil {
		t.Fatal(err)
	}
	w, err := db.GetCronWatch(custID, name)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, v := range values {
		err := db.SaveCronResult(&store.CronResult{
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

	wf := &WeftFuncs{Store: db, CustomerID: custID}
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
	wf := &WeftFuncs{Store: db, CustomerID: custID}
	_, err := wf.Latest("nonexistent-watch")
	if err == nil {
		t.Error("expected error for nonexistent watch")
	}
}

func TestWeftFuncs_Avg(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "avg-test", []string{"10", "20", "30"}, time.Minute)

	wf := &WeftFuncs{Store: db, CustomerID: custID}
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

	wf := &WeftFuncs{Store: db, CustomerID: custID}
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

	wf := &WeftFuncs{Store: db, CustomerID: custID}
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

	wf := &WeftFuncs{Store: db, CustomerID: custID}
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

	wf := &WeftFuncs{Store: db, CustomerID: custID}
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

	wf := &WeftFuncs{Store: db, CustomerID: custID}
	_, err := wf.Stddev("stddev-one", 1*time.Hour)
	if err == nil {
		t.Error("expected error for single result")
	}
}

func TestWeftFuncs_SkipsNonNumeric(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "mixed-test", []string{"10", "not-a-number", "30", "ERROR", "20"}, time.Minute)

	wf := &WeftFuncs{Store: db, CustomerID: custID}

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
	wf := &WeftFuncs{Store: db, CustomerID: custID}

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
	err := db.SaveCronWatch(custID, "range-filter-test", "every 1m", "echo test", "")
	if err != nil {
		t.Fatal(err)
	}
	w, _ := db.GetCronWatch(custID, "range-filter-test")
	now := time.Now()

	// Old values (2h ago) — should be excluded from 30m query.
	for _, v := range []string{"100", "200"} {
		db.SaveCronResult(&store.CronResult{
			CronWatchID: w.ID,
			RanAt:       now.Add(-2 * time.Hour),
			Stdout:      v,
		})
	}
	// Recent values (5m ago) — should be included.
	for _, v := range []string{"10", "20"} {
		db.SaveCronResult(&store.CronResult{
			CronWatchID: w.ID,
			RanAt:       now.Add(-5 * time.Minute),
			Stdout:      v,
		})
	}

	wf := &WeftFuncs{Store: db, CustomerID: custID}
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

	wf := &WeftFuncs{Store: db, CustomerID: custID}

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
	db.SaveCronWatch(custID, "empty-test", "every 1m", "echo test", "")

	wf := &WeftFuncs{Store: db, CustomerID: custID}

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
		{"25h", 0, true},  // exceeds cap
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
