package store

import (
	"errors"
	"fmt"
	"time"
)

// ColdScanAllowance is the lines a tier may scan from cold storage in a
// calendar month: one full pass over the tier's cold archive, at the plan's
// full ingest. Derived from scripts/costmodel (tiers.yaml scan_passes = 1,
// 155 bytes per row, a 14-day hot window, 30-day months); TestColdScanAllowance
// pins the values. Trial has no cold history to read.
func ColdScanAllowance(tier Tier) int64 {
	switch tier {
	case TierSwatch:
		return 34_400_000
	case TierYard:
		return 1_634_000_000
	case TierBolt:
		return 17_850_000_000
	default:
		return 0
	}
}

// ErrColdScanBudgetExhausted is returned, wrapped in a *ColdScanBudgetError,
// when a query needs cold data and the month's scan budget is spent.
var ErrColdScanBudgetExhausted = errors.New("cold scan budget exhausted")

// ColdScanBudgetError says how much of the month's cold history budget was
// used and when it resets. It matches ErrColdScanBudgetExhausted.
type ColdScanBudgetError struct {
	Used, Allowance int64
	ResetsAt        time.Time
}

func (e *ColdScanBudgetError) Error() string {
	return fmt.Sprintf("this month's cold history budget is used (%s of %s lines); queries over recent data still work, and it resets on %s",
		FormatLines(e.Used), FormatLines(e.Allowance), e.ResetsAt.Format("2 January"))
}

func (e *ColdScanBudgetError) Is(target error) bool { return target == ErrColdScanBudgetExhausted }

// FormatLines renders a line count for people: 34.4 million, 1.63 billion.
func FormatLines(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2f billion", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1f million", float64(n)/1e6)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// ColdScanPeriod is the calendar month (UTC) containing t, the same period
// the ingest allowance uses.
func ColdScanPeriod(t time.Time) (start, end time.Time) {
	t = t.UTC()
	start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// QueryColdScan returns the lines an org scanned from cold storage in
// [periodStart, periodEnd).
func (s *Store) QueryColdScan(orgID OrgID, periodStart, periodEnd time.Time) (int64, error) {
	var result struct{ Total int64 }
	err := s.db.Raw(`
		SELECT COALESCE(SUM(rows_scanned), 0) AS total FROM cold_scan_daily
		WHERE org_id = ? AND day >= ?::date AND day < ?::date`,
		orgID, periodStart, periodEnd).Scan(&result).Error
	return result.Total, err
}

// AddColdScan appends one delta of scanned lines for an org and day.
func (s *Store) AddColdScan(orgID int64, day time.Time, rows int64) error {
	return s.db.Exec(`INSERT INTO cold_scan_daily (org_id, day, rows_scanned) VALUES (?, ?::date, ?)`,
		orgID, day, rows).Error
}
