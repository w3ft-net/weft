package shard

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/logtable"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
)

// A shard in the per-key layout keeps each (customer, namespace, service,
// kind) in a table of its own; docs/plans/per-service-tables.md.

type pkKey struct {
	customer           int64
	namespace, service string
	kind               string
}

func (k pkKey) table() string { return logtable.Name([]any{k.customer}, k.namespace, k.service, k.kind) }

func pkRows(k pkKey, n int, tag string) []*pb.LogRow {
	rows := make([]*pb.LogRow, n)
	for i := range rows {
		rows[i] = &pb.LogRow{
			Raw: fmt.Sprintf("%s/%s %s line %d", k.service, k.kind, tag, i), Namespace: k.namespace, Service: k.service,
			Kind: k.kind, CustomerId: k.customer, TsUnixNs: int64(1_700_000_000+i) * 1e9, Level: "INFO",
		}
	}
	return rows
}

func openPerKey(t *testing.T, path string) *DuckDBShard {
	t.Helper()
	s, err := NewDuckDBShard("pk", path, WithPerKeyTables())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func countRows(t *testing.T, s *DuckDBShard, from string) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + from).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var (
	pkStats  = pkKey{1, "weft", "shard", "stats"}
	pkMain   = pkKey{1, "weft", "shard", "main"}
	pkOther  = pkKey{2, "acme", "api", "main"}
	pkAbsent = pkKey{1, "weft", "nothing", "main"}
)

func ingestAll(t *testing.T, s *DuckDBShard) {
	t.Helper()
	var batch []*pb.LogRow
	// interleaved, as a real batch is
	for i := 0; i < 5; i++ {
		batch = append(batch, pkRows(pkStats, 1, fmt.Sprint(i))...)
		batch = append(batch, pkRows(pkMain, 2, fmt.Sprint(i))...)
		batch = append(batch, pkRows(pkOther, 3, fmt.Sprint(i))...)
	}
	if err := s.Ingest(context.Background(), batch, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPerKey_RowsLandInTheirOwnTables(t *testing.T) {
	s := openPerKey(t, filepath.Join(t.TempDir(), "s.duckdb"))
	if !s.PerKey() {
		t.Fatal("a fresh shard opened WithPerKeyTables is not per-key")
	}
	ingestAll(t, s)

	for _, c := range []struct {
		k    pkKey
		want int64
	}{{pkStats, 5}, {pkMain, 10}, {pkOther, 15}} {
		if got := countRows(t, s, `"`+c.k.table()+`"`); got != c.want {
			t.Errorf("%v: table holds %d rows, want %d", c.k, got, c.want)
		}
	}
	if got := countRows(t, s, "logs"); got != 0 {
		t.Errorf("the logs template holds %d rows, want 0", got)
	}
	if got := s.RowCount(); got != 30 {
		t.Errorf("RowCount = %d, want 30", got)
	}
}

func TestPerKey_HintedQueryReadsOnlyItsTable(t *testing.T) {
	s := openPerKey(t, filepath.Join(t.TempDir(), "s.duckdb"))
	ingestAll(t, s)
	q := "SELECT raw, ts FROM logs WHERE customer_id = ?"

	rows, err := s.QueryContextTable(context.Background(), pkStats.table(), q, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	// customer 1 has 15 rows across two tables; the hint narrows it to stats
	if len(rows) != 5 {
		t.Fatalf("hinted query returned %d rows, want 5 (the stats table's)", len(rows))
	}
	for _, r := range rows {
		if !strings.HasPrefix(r.Raw, "shard/stats ") {
			t.Errorf("row %q is not from the hinted table", r.Raw)
		}
	}
}

func TestPerKey_HintForAKeyWithNoTableReturnsNothing(t *testing.T) {
	s := openPerKey(t, filepath.Join(t.TempDir(), "s.duckdb"))
	ingestAll(t, s)
	rows, err := s.QueryContextTable(context.Background(), pkAbsent.table(), "SELECT raw, ts FROM logs")
	if err != nil {
		t.Fatalf("a key with no rows must read as empty, got error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}

// A query that names no table (the planner has not been taught the source, a
// tool, an older coordinator) still sees every row.
func TestPerKey_UnhintedQuerySeesEveryTable(t *testing.T) {
	s := openPerKey(t, filepath.Join(t.TempDir(), "s.duckdb"))
	ingestAll(t, s)
	rows, err := s.QueryContext(context.Background(), "SELECT raw, ts FROM logs")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 30 {
		t.Fatalf("unhinted query returned %d rows, want 30", len(rows))
	}
	rows, err = s.QueryContext(context.Background(), "SELECT raw, ts FROM logs WHERE namespace = ? AND service = ?", "acme", "api")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 15 {
		t.Fatalf("filtered unhinted query returned %d rows, want 15", len(rows))
	}
}

func TestPerKey_NestedAndAliasedFromStillResolve(t *testing.T) {
	s := openPerKey(t, filepath.Join(t.TempDir(), "s.duckdb"))
	ingestAll(t, s)
	q := "SELECT '' AS raw, min(ts) AS ts, count(*) AS count FROM (SELECT raw, ts FROM logs AS l WHERE l.service = ?) AS t"
	for _, table := range []string{"", pkMain.table()} {
		rows, err := s.QueryContextTable(context.Background(), table, q, "shard")
		if err != nil {
			t.Fatalf("table=%q: %v", table, err)
		}
		if len(rows) != 1 {
			t.Fatalf("table=%q: %d rows", table, len(rows))
		}
	}
}

func TestPerKey_SurvivesReopenAndStaysPerKeyWithoutTheOption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.duckdb")
	s := openPerKey(t, path)
	ingestAll(t, s)
	s.Close()

	// A shard on a volume is reopened by whatever the binary's default is.
	r, err := NewDuckDBShard("pk", path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if !r.PerKey() {
		t.Fatal("a per-key database reopened without the option fell back to the legacy layout")
	}
	if got := r.RowCount(); got != 30 {
		t.Errorf("RowCount after reopen = %d, want 30", got)
	}
	rows, err := r.QueryContextTable(context.Background(), pkStats.table(), "SELECT raw, ts FROM logs")
	if err != nil || len(rows) != 5 {
		t.Fatalf("after reopen: rows=%d err=%v, want 5", len(rows), err)
	}
	// and it keeps writing to the same tables
	if err := r.Ingest(context.Background(), pkRows(pkStats, 2, "again"), nil); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, r, `"`+pkStats.table()+`"`); got != 7 {
		t.Errorf("stats table holds %d rows after a second ingest, want 7", got)
	}
}

func TestPerKey_ADatabaseWithRowsStaysLegacyEvenWithTheOption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.duckdb")
	old, err := NewDuckDBShard("old", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Ingest(context.Background(), pkRows(pkStats, 4, "legacy"), nil); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s := openPerKey(t, path)
	if s.PerKey() {
		t.Fatal("a database that already holds rows in logs was switched to per-key")
	}
	if err := s.Ingest(context.Background(), pkRows(pkMain, 3, "more"), nil); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, s, "logs"); got != 7 {
		t.Errorf("logs holds %d rows, want 7", got)
	}
	// a hint means nothing to a legacy shard: the query runs as written
	rows, err := s.QueryContextTable(context.Background(), pkAbsent.table(), "SELECT raw, ts FROM logs")
	if err != nil || len(rows) != 7 {
		t.Fatalf("legacy shard with a hint: rows=%d err=%v, want 7", len(rows), err)
	}
}

func TestLegacyShardIsTheDefault(t *testing.T) {
	s, err := NewDuckDBShard("old", filepath.Join(t.TempDir(), "s.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.PerKey() {
		t.Fatal("a shard opened without the option is per-key")
	}
}

// One bad row anywhere in a batch must leave nothing from the batch behind:
// a caller that retries the batch would otherwise store its good rows twice.
func TestPerKey_AFailedBatchStoresNothing(t *testing.T) {
	s := openPerKey(t, filepath.Join(t.TempDir(), "s.duckdb"))
	batch := append(pkRows(pkStats, 3, "good"), pkRows(pkOther, 3, "bad")...)
	batch[len(batch)-1].EventId = "not-a-uuid"
	if err := s.Ingest(context.Background(), batch, nil); err == nil {
		t.Fatal("ingest of a batch with a malformed event_id succeeded")
	}
	if got := s.RowCount(); got != 0 {
		t.Errorf("RowCount = %d after a failed batch, want 0", got)
	}
	rows, err := s.QueryContext(context.Background(), "SELECT raw, ts FROM logs")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("%d rows stored from a failed batch, want 0", len(rows))
	}
}

// The customer of a key comes from the batch's partition values, not from the
// rows: a coordinator that no longer stamps customer_id still lands rows in
// their customer's table and records their facts under it.
func TestPerKey_CustomerComesFromThePartitionNotTheRows(t *testing.T) {
	s := openPerKey(t, filepath.Join(t.TempDir(), "s.duckdb"))
	k := pkKey{9, "acme", "api", "main"}
	rows := pkRows(k, 4, "x")
	for _, r := range rows {
		r.CustomerId = 0
	}
	if err := s.IngestBatch(context.Background(), Batch{Rows: rows, PartitionValues: []string{"9"}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, s, `"`+k.table()+`"`); got != 4 {
		t.Fatalf("customer 9's table holds %d rows, want 4", got)
	}
	s.facts.Drain()
	s.facts.mu.Lock()
	defer s.facts.mu.Unlock()
	if len(s.facts.sources) == 0 {
		t.Error("no fact source recorded")
	}
	for sk := range s.facts.sources {
		if sk.customerID != 9 {
			t.Errorf("fact source %+v is not under customer 9", sk)
		}
	}
}

// A new column added to the schema must reach the tables that already exist.
func TestPerKey_ReopenAddsColumnsATableLacks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.duckdb")
	s := openPerKey(t, path)
	ingestAll(t, s)
	if _, err := s.db.Exec(fmt.Sprintf(`ALTER TABLE "%s" DROP COLUMN pod`, pkStats.table())); err != nil {
		t.Fatal(err)
	}
	s.Close()

	r, err := NewDuckDBShard("pk", path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Ingest(context.Background(), pkRows(pkStats, 1, "after"), nil); err != nil {
		t.Fatalf("ingest into a table that lost a column and was reopened: %v", err)
	}
}

// A shard too small to partition is archived as one file by a separate branch
// of the export (the one used without S3). It must read the per-key tables too,
// not the empty logs template, or the archive is empty and reports success.
func TestPerKey_SmallLocalArchiveHoldsEveryRow(t *testing.T) {
	s := openPerKey(t, filepath.Join(t.TempDir(), "s.duckdb"))
	ingestAll(t, s)

	path, stats, err := s.Archive(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, c := range stats {
		total += c.RowCount
	}
	if total != 30 {
		t.Errorf("archive stats count %d rows, want 30", total)
	}
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int64
	if err := db.QueryRow("SELECT count(*) FROM " + ParquetReadExpr(path, shape.Legacy())).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 30 {
		t.Errorf("archive file %s holds %d rows, want 30", path, n)
	}
}

// The sized export of a per-key database, opened the way the export host opens a
// restored volume: it must write every key's rows, from the tables, not the
// empty template. The legacy case is covered in keyed_archive_test.go.
func TestPerKey_SizedExportWritesEveryKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.duckdb")
	s := openPerKey(t, path)
	ingestAll(t, s)
	s.Close()

	e, err := OpenForExport("pk", path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if !e.PerKey() {
		t.Fatal("the export host did not see the per-key layout")
	}
	dir := filepath.Join(t.TempDir(), "shard.attempt-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	glob, n, err := e.exportKeyedParquet(context.Background(), dir, smallShardBytes)
	if err != nil {
		t.Fatal(err)
	}
	if n != 30 || !IsSized(glob) {
		t.Fatalf("export wrote %d rows to %q, want 30 to a sized archive", n, glob)
	}

	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, c := range []struct {
		k    pkKey
		want int
	}{{pkStats, 5}, {pkMain, 10}, {pkOther, 15}} {
		var got int
		q := "SELECT count(*) FROM " + ParquetReadExpr(glob, shape.Legacy()) + " WHERE customer_id = CAST(? AS BIGINT) AND namespace = ? AND service = ? AND kind = ?"
		if err := db.QueryRow(q, fmt.Sprint(c.k.customer), c.k.namespace, c.k.service, c.k.kind).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%v read back %d rows, want %d", c.k, got, c.want)
		}
		if dirs, _ := filepath.Glob(filepath.Join(dir, "sized", fmt.Sprintf("customer_id=%d", c.k.customer), "namespace="+c.k.namespace,
			"service="+c.k.service, "kind="+c.k.kind)); len(dirs) != 1 {
			t.Errorf("%v has %d directories, want 1", c.k, len(dirs))
		}
	}
}

// A table that lost a column and got it back on reopen has it last, so the
// tables of a shard disagree on column order. Reading them together, as the
// export does, must match columns by name.
func TestPerKey_ExportMatchesColumnsByNameAcrossTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.duckdb")
	s := openPerKey(t, path)
	ingestAll(t, s)
	if _, err := s.db.Exec(fmt.Sprintf(`ALTER TABLE "%s" DROP COLUMN pod`, pkStats.table())); err != nil {
		t.Fatal(err)
	}
	s.Close()
	r, err := NewDuckDBShard("pk", path)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()

	e, err := OpenForExport("pk", path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	out := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, n, err := e.exportParquet(context.Background(), out, 0); err != nil || n != 30 {
		t.Fatalf("export wrote %d rows, err=%v, want 30", n, err)
	}
}

// The archive export opens the database without NewDuckDBShard. If it did not
// see the per-key tables it would export the empty template and report success.
func TestPerKey_ExportSeesEveryRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.duckdb")
	s := openPerKey(t, path)
	ingestAll(t, s)
	s.Close()

	e, err := OpenForExport("pk", path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, minutes := range []int{0, 5} {
		out := filepath.Join(t.TempDir(), "out")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		_, n, err := e.exportParquet(context.Background(), out, minutes)
		if err != nil {
			t.Fatalf("minutes=%d: %v", minutes, err)
		}
		if n != 30 {
			t.Errorf("minutes=%d: export wrote %d rows, want 30", minutes, n)
		}
	}
	stats, err := e.customerStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, c := range stats {
		total += c.RowCount
	}
	if len(stats) != 2 || total != 30 {
		t.Errorf("customerStats = %+v, want 2 customers and 30 rows", stats)
	}
}
