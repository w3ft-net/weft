package store

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// coldScanFormula is scripts/costmodel's cold line scan budget at one pass:
// ingest GB x 1e9 / 155 bytes per row x (retention - 14 hot days) / 30.
func coldScanFormula(ingestGB float64, retentionDays float64) float64 {
	return ingestGB * 1e9 / 155 * (retentionDays - 14) / 30
}

// TestColdScanAllowance pins the monthly cold line budgets and checks them
// against the cost model's formula, so a change to a tier's ingest or
// retention that skips the budget fails here.
func TestColdScanAllowance(t *testing.T) {
	cases := []struct {
		tier Tier
		want int64
		gb   float64
		days float64
	}{
		{TierSwatch, 34_400_000, 10, 30},
		{TierYard, 1_634_000_000, 100, 90},
		{TierBolt, 17_850_000_000, 500, 180},
	}
	for _, c := range cases {
		if got := ColdScanAllowance(c.tier); got != c.want {
			t.Errorf("ColdScanAllowance(%q) = %d, want %d", c.tier, got, c.want)
		}
		f := coldScanFormula(c.gb, c.days)
		if math.Abs(float64(c.want)-f)/f > 0.01 {
			t.Errorf("%s: %d is more than 1%% from the model's %.0f; see scripts/costmodel/tiers.yaml", c.tier, c.want, f)
		}
		if got := LogRetention(c.tier); got != time.Duration(c.days)*24*time.Hour {
			t.Errorf("%s: retention %v no longer matches the %v days the budget assumes", c.tier, got, c.days)
		}
		if got := VolumeAllowance(c.tier); got != int64(c.gb)*1_000_000_000 {
			t.Errorf("%s: volume allowance %d no longer matches the %v GB the budget assumes", c.tier, got, c.gb)
		}
	}
	for _, tier := range []Tier{TierTrial, ""} {
		if got := ColdScanAllowance(tier); got != 0 {
			t.Errorf("ColdScanAllowance(%q) = %d, want 0", tier, got)
		}
	}
}

func TestColdScanBudgetError(t *testing.T) {
	var err error = &ColdScanBudgetError{
		Used: 1_634_000_000, Allowance: 1_634_000_000,
		ResetsAt: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
	}
	if !errors.Is(err, ErrColdScanBudgetExhausted) {
		t.Fatal("ColdScanBudgetError does not match ErrColdScanBudgetExhausted")
	}
	msg := err.Error()
	for _, want := range []string{"1.63 billion of 1.63 billion", "1 December", "recent data still work"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	for _, banned := range []string{"shard", "lambda", "parquet", "sql"} {
		if strings.Contains(strings.ToLower(msg), banned) {
			t.Errorf("message %q uses internal vocabulary %q", msg, banned)
		}
	}
}

func TestColdScanPeriod(t *testing.T) {
	start, end := ColdScanPeriod(time.Date(2026, 11, 8, 23, 59, 0, 0, time.FixedZone("x", -5*3600)))
	if !start.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("period = %v..%v", start, end)
	}
}

// TestQueryColdScan sums appended deltas inside the period only, per org.
func TestQueryColdScan(t *testing.T) {
	db := testOpen(t)
	a := mustOrg(t, db, "coldscan-a")
	b := mustOrg(t, db, "coldscan-b")
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }
	for _, add := range []struct {
		org  OrgID
		day  time.Time
		rows int64
	}{
		{a, day(11, 2), 100}, {a, day(11, 2), 50}, {a, day(11, 30), 7},
		{a, day(10, 31), 1000}, {a, day(12, 1), 2000}, {b, day(11, 2), 9},
	} {
		if err := db.AddColdScan(int64(add.org), add.day, add.rows); err != nil {
			t.Fatal(err)
		}
	}
	start, end := ColdScanPeriod(day(11, 15))
	got, err := db.QueryColdScan(a, start, end)
	if err != nil || got != 157 {
		t.Errorf("org a = %d, %v; want 157", got, err)
	}
	if got, _ := db.QueryColdScan(b, start, end); got != 9 {
		t.Errorf("org b = %d, want 9", got)
	}
}
