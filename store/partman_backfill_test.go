package store

import (
	"sort"
	"testing"
)

// TestPartmanBackfill pins the reproducibility fix: a freshly
// bootstrapped database must end up with partman configured for every
// managed table. Before migration 050 a fresh DB had the partitioned
// tables but ZERO partman.part_config rows (pg_dump can't serialize
// create_parent calls and the collapsed migrations that ran them were
// deleted) — so partman/retention was inert on every non-prod env.
//
// 050 reconstructs create_parent for every managed table (shard_heartbeats
// left that set in migration 081). The cron
// jobs are NOT asserted here: pg_cron only exists in the prod cron
// database, so on weft_test the cron.schedule block is expected to be
// skipped (tolerated) — see migration 050's guarded DO block.
func TestPartmanBackfill(t *testing.T) {
	db := testOpen(t)

	want := []string{
		"public.cron_results",
		"public.cron_results_4h",
		"public.cron_results_daily",
		"public.cron_results_hourly",
		"public.tmp_file_lines",
		"public.tmp_files",
	}

	var got []string
	if err := db.db.Raw(
		`SELECT parent_table FROM partman.part_config ORDER BY parent_table`,
	).Scan(&got).Error; err != nil {
		t.Fatalf("read part_config: %v", err)
	}
	sort.Strings(got)

	missing := map[string]bool{}
	for _, w := range want {
		missing[w] = true
	}
	for _, g := range got {
		delete(missing, g)
	}
	if len(missing) > 0 {
		t.Fatalf("part_config missing managed tables after bootstrap: %v (got %v) — "+
			"migration 050 did not reconstruct create_parent", keys(missing), got)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
