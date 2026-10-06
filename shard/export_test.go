package shard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// exportFixtureDB builds a checkpointed on-disk shard file, the shape the
// export instance finds on the volume restored from a snapshot.
func exportFixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shard.duckdb")
	s, err := NewDuckDBShard("s-export", path)
	if err != nil {
		t.Fatal(err)
	}
	base := int64(1_700_000_000) * int64(time.Second)
	rows := []*pb.LogRow{
		{Raw: "a1", CustomerId: 5, TsUnixNs: base},
		{Raw: "a2", CustomerId: 5, TsUnixNs: base + int64(time.Hour)},
		{Raw: "g1", CustomerId: 6, TsUnixNs: base + int64(30*time.Minute)},
	}
	if err := s.Ingest(rows, nil); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// recordingManifestStore captures each Put and what the parquet output looked
// like at that instant, so a test can prove the manifest is written last.
type recordingManifestStore struct {
	put          []ExportManifest
	parquetDir   string
	parquetAtPut bool
	err          error
}

func (r *recordingManifestStore) Put(_ context.Context, _ string, m ExportManifest) error {
	if r.err != nil {
		return r.err
	}
	matches, _ := filepath.Glob(filepath.Join(r.parquetDir, "*.parquet"))
	r.parquetAtPut = len(matches) > 0
	r.put = append(r.put, m)
	return nil
}

func TestRunExport_WritesParquetThenManifest(t *testing.T) {
	db := exportFixtureDB(t)
	out := t.TempDir()
	// Local mode names the single-file parquet <prefix>/<id>.parquet.
	store := &recordingManifestStore{parquetDir: out}

	m, err := RunExport(context.Background(), ExportConfig{
		ShardID: "s-export", DBPath: db, S3Prefix: out, Attempt: 3,
	}, store)
	if err != nil {
		t.Fatalf("RunExport: %v", err)
	}
	if !store.parquetAtPut {
		t.Fatal("manifest was written before the parquet output existed")
	}
	if len(store.put) != 1 {
		t.Fatalf("manifest puts = %d, want 1", len(store.put))
	}
	if m.ShardID != "s-export" || m.Attempt != 3 {
		t.Fatalf("manifest identity = %+v", m)
	}
	if m.Rows != 3 {
		t.Fatalf("manifest rows = %d, want 3", m.Rows)
	}
	if m.DBBytes <= 0 {
		t.Fatalf("manifest db bytes = %d, want >0", m.DBBytes)
	}
	if len(m.Customers) != 2 {
		t.Fatalf("manifest customers = %+v, want 2", m.Customers)
	}
	if m.ParquetPath == "" || m.Finished.Before(m.Started) {
		t.Fatalf("manifest path/timing wrong: %+v", m)
	}
	if got := store.put[0]; got.ParquetPath != m.ParquetPath {
		t.Fatalf("stored manifest differs from returned: %+v vs %+v", got, m)
	}
}

// A snapshot taken without a clean checkpoint is a crash-consistent copy: the
// main file plus an un-replayed WAL. The export must still see every row. This
// is what lets the CHECKPOINT before the snapshot be best-effort.
func TestRunExport_ReplaysWALFromCrashConsistentCopy(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "shard.duckdb")
	live, err := NewDuckDBShard("s-export", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	base := int64(1_700_000_000) * int64(time.Second)
	var rows []*pb.LogRow
	for i := 0; i < 50; i++ {
		rows = append(rows, &pb.LogRow{Raw: "r", CustomerId: 5, TsUnixNs: base + int64(i)*int64(time.Second)})
	}
	if err := live.Ingest(rows, nil); err != nil {
		t.Fatal(err)
	}

	// Copy while the shard is still open and un-checkpointed.
	copyDir := t.TempDir()
	copied := 0
	for _, name := range []string{"shard.duckdb", "shard.duckdb.wal"} {
		b, err := os.ReadFile(filepath.Join(srcDir, name))
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(copyDir, name), b, 0644); err != nil {
			t.Fatal(err)
		}
		copied++
	}
	if _, err := os.Stat(filepath.Join(copyDir, "shard.duckdb.wal")); err != nil {
		t.Skipf("DuckDB left no WAL for this ingest (copied %d files); nothing to replay", copied)
	}

	m, err := RunExport(context.Background(), ExportConfig{
		ShardID: "s-export", DBPath: filepath.Join(copyDir, "shard.duckdb"), S3Prefix: t.TempDir(), Attempt: 1,
	}, LocalManifestStore{})
	if err != nil {
		t.Fatalf("RunExport on a crash-consistent copy: %v", err)
	}
	if m.Rows != 50 {
		t.Fatalf("exported %d rows from the WAL-bearing copy, want 50", m.Rows)
	}
}

// An export that fails must leave no manifest: the coordinator treats a
// manifest as proof the Parquet is complete.
func TestRunExport_NoManifestOnFailure(t *testing.T) {
	db := exportFixtureDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &recordingManifestStore{parquetDir: t.TempDir()}

	_, err := RunExport(ctx, ExportConfig{ShardID: "s-export", DBPath: db, S3Prefix: t.TempDir(), Attempt: 1}, store)
	if err == nil {
		t.Fatal("RunExport with a cancelled context returned nil error")
	}
	if len(store.put) != 0 {
		t.Fatalf("manifest written for a failed export: %+v", store.put)
	}
}

func TestRunExport_ManifestStoreFailureIsAnError(t *testing.T) {
	db := exportFixtureDB(t)
	store := &recordingManifestStore{parquetDir: t.TempDir(), err: errors.New("s3 down")}
	if _, err := RunExport(context.Background(), ExportConfig{ShardID: "s-export", DBPath: db, S3Prefix: t.TempDir(), Attempt: 1}, store); err == nil {
		t.Fatal("a failed manifest write must fail the export (no manifest = not done)")
	}
}

// A missing or empty database must never export as a "successful" empty
// archive: the coordinator would then retire the real volume.
func TestOpenForExport_RefusesMissingOrEmptyDB(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.duckdb")
	if _, err := OpenForExport("s", missing); err == nil {
		t.Fatal("OpenForExport on a missing file succeeded")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("OpenForExport created the missing database file")
	}

	// A valid DuckDB file with no logs table is also refused, and no table
	// is created in it.
	empty := filepath.Join(t.TempDir(), "empty.duckdb")
	seed, err := NewDuckDBShard("seed", empty)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.db.Exec("DROP TABLE logs"); err != nil {
		t.Fatal(err)
	}
	seed.Close()
	if _, err := OpenForExport("s", empty); err == nil {
		t.Fatal("OpenForExport on a database without a logs table succeeded")
	}
}

func TestExportManifest_JSONRoundTrip(t *testing.T) {
	in := ExportManifest{
		ShardID: "s", Attempt: 2, ParquetPath: "s3://b/p/s/**/*.parquet", Rows: 7, DBBytes: 99,
		Customers: []CustomerStat{{CustomerID: 5, RowCount: 7}},
		Started:   time.Unix(100, 0).UTC(), Finished: time.Unix(200, 0).UTC(),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ExportManifest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.ShardID != in.ShardID || out.Attempt != 2 || out.Rows != 7 || len(out.Customers) != 1 || !out.Finished.Equal(in.Finished) {
		t.Fatalf("round trip lost data: %+v", out)
	}
}

func TestLocalManifestStore_GetRoundTripAndMissing(t *testing.T) {
	dir := t.TempDir()
	key := ManifestKey(dir, "s-1")
	var store LocalManifestStore

	if _, found, err := store.Get(context.Background(), key); err != nil || found {
		t.Fatalf("missing manifest: found=%v err=%v, want not found and no error", found, err)
	}

	in := ExportManifest{ShardID: "s-1", Attempt: 4, ParquetPath: "p", Rows: 9, Customers: []CustomerStat{{CustomerID: 2, RowCount: 9}}}
	if err := store.Put(context.Background(), key, in); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.Get(context.Background(), key)
	if err != nil || !found {
		t.Fatalf("Get after Put: found=%v err=%v", found, err)
	}
	if got.Attempt != 4 || got.Rows != 9 || len(got.Customers) != 1 {
		t.Fatalf("round trip lost data: %+v", got)
	}

	// A corrupt manifest is an error, not "not found": the coordinator must not
	// treat garbage as either proof of completion or proof of absence.
	if err := os.WriteFile(key, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Get(context.Background(), key); err == nil || found {
		t.Fatalf("corrupt manifest: found=%v err=%v, want an error", found, err)
	}
}

// A retry must never share an output location with a failed attempt:
// DuckDB's OVERWRITE_OR_IGNORE only replaces same-named files, so a file left by
// an earlier partial attempt survives and the archive glob reads it too (the
// rows double-count). Each attempt therefore writes to its own location.
func TestRunExport_EachAttemptWritesItsOwnOutput(t *testing.T) {
	db := exportFixtureDB(t)
	out := t.TempDir()
	run := func(attempt int) ExportManifest {
		t.Helper()
		m, err := RunExport(context.Background(), ExportConfig{ShardID: "s-export", DBPath: db, S3Prefix: out, Attempt: attempt}, LocalManifestStore{})
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		return m
	}

	first := run(1)
	second := run(2)

	if first.ParquetPath == second.ParquetPath {
		t.Fatalf("attempts 1 and 2 share an output path: %s", first.ParquetPath)
	}
	if !strings.Contains(first.ParquetPath, ".attempt-1") || !strings.Contains(second.ParquetPath, ".attempt-2") {
		t.Fatalf("output paths do not name their attempt: %q, %q", first.ParquetPath, second.ParquetPath)
	}
	// The first attempt's output is untouched by the second.
	if _, err := os.Stat(first.ParquetPath); err != nil {
		t.Fatalf("attempt 1's output was disturbed by attempt 2: %v", err)
	}
	// Reading attempt 2's path alone yields exactly the shard's rows, even with
	// attempt 1's files sitting next to it.
	s, err := NewDuckDBShard("reader", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM read_parquet('" + second.ParquetPath + "')").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("attempt 2's output holds %d rows, want 3", n)
	}
}

// The in-process Archive keeps its original layout; only the export is
// attempt-scoped.
func TestDuckDBShard_ArchiveKeepsItsOriginalLayout(t *testing.T) {
	s := archiveFixtureShard(t)
	out := t.TempDir()
	path, _, err := s.Archive(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(out, "phase-test.parquet"); path != want {
		t.Fatalf("Archive path = %q, want %q", path, want)
	}
}

func TestExportOutputPaths(t *testing.T) {
	got := ExportOutputPaths("s3://weft/warp/", "s-1", 2)
	want := []string{"s3://weft/warp/s-1.attempt-2/**/*.parquet", "s3://weft/warp/s-1.attempt-2.parquet"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ExportOutputPaths = %v, want %v", got, want)
	}
	// Attempt 1 must not be able to match attempt 10's output.
	for _, p := range ExportOutputPaths("s3://b/p", "s-1", 1) {
		if strings.Contains(p, "attempt-10") || strings.HasSuffix(p, "attempt-1") {
			t.Errorf("path %q is not exact enough to be a delete target", p)
		}
	}
}

func TestManifestKey(t *testing.T) {
	if got := ManifestKey("s3://bkt/warp", "s-1"); got != "s3://bkt/warp/_manifests/s-1.json" {
		t.Fatalf("ManifestKey = %q", got)
	}
	if got := ManifestKey("s3://bkt/warp/", "s-1"); got != "s3://bkt/warp/_manifests/s-1.json" {
		t.Fatalf("ManifestKey with trailing slash = %q", got)
	}
}

func TestParseS3URL(t *testing.T) {
	b, k, err := parseS3URL("s3://bkt/a/b/c.json")
	if err != nil || b != "bkt" || k != "a/b/c.json" {
		t.Fatalf("got %q %q %v", b, k, err)
	}
	for _, bad := range []string{"", "bkt/a", "s3://", "s3://bkt", "http://bkt/a"} {
		if _, _, err := parseS3URL(bad); err == nil {
			t.Errorf("parseS3URL(%q) succeeded, want error", bad)
		}
	}
}

func TestLocalManifestStore_PutWritesJSONAtKey(t *testing.T) {
	dir := t.TempDir()
	m := ExportManifest{ShardID: "s-1", Attempt: 1, Rows: 4}
	if err := (LocalManifestStore{}).Put(context.Background(), ManifestKey(dir, "s-1"), m); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "_manifests", "s-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got ExportManifest
	if err := json.Unmarshal(b, &got); err != nil || got.Rows != 4 {
		t.Fatalf("stored manifest = %s (%v)", b, err)
	}
}
