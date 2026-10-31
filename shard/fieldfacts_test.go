package shard

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

func numRows(n int, raw string) []*pb.LogRow {
	rows := make([]*pb.LogRow, n)
	for i := range rows {
		rows[i] = &pb.LogRow{Raw: raw, CustomerId: 1, Namespace: "ns", Service: "svc", Env: "prod"}
	}
	return rows
}

// settle waits for the classifier to finish what Observe queued.
func settle(t *testing.T, c *FieldClassifier) {
	t.Helper()
	c.inflight.Wait()
}

func factFor(facts []FieldFact, locator string) (FieldFact, bool) {
	for _, f := range facts {
		if f.Locator == locator {
			return f, true
		}
	}
	return FieldFact{}, false
}

func TestFieldClassifier_TypedAfterEnoughNumericRows(t *testing.T) {
	c := NewFieldClassifier()
	defer c.Stop()

	c.Observe(numRows(factTypedAfter-1, "GET /a 200 12.5"))
	settle(t, c)
	if got := c.Pending(); len(got) != 0 {
		t.Fatalf("typed before %d rows: %+v", factTypedAfter, got)
	}

	c.Observe(numRows(1, "GET /a 200 12.5"))
	settle(t, c)
	facts := c.Pending()
	if f, ok := factFor(facts, "$3"); !ok || f.State != FactTyped || f.Env != "prod" || f.CustomerID != 1 {
		t.Errorf("$3 want typed for customer 1 env prod, got %+v (ok=%v)", f, ok)
	}
	if f, ok := factFor(facts, "$4"); !ok || f.State != FactTyped {
		t.Errorf("$4 (a float) want typed, got %+v (ok=%v)", f, ok)
	}
	for _, loc := range []string{"$1", "$2"} {
		if _, ok := factFor(facts, loc); ok {
			t.Errorf("%s holds text and must never be reported", loc)
		}
	}
}

func TestFieldClassifier_TypedThenViolationReportsDirty(t *testing.T) {
	c := NewFieldClassifier()
	defer c.Stop()
	c.Observe(numRows(factTypedAfter, "x 7"))
	settle(t, c)
	typed, _ := factFor(c.Pending(), "$2")
	c.Ack(typed.Epoch)
	if got := c.Pending(); len(got) != 0 {
		t.Fatalf("acked fact still pending: %+v", got)
	}

	c.Observe(numRows(1, "x 12abc"))
	settle(t, c)
	f, ok := factFor(c.Pending(), "$2")
	if !ok || f.State != FactDirty || f.Epoch <= typed.Epoch {
		t.Errorf("want a newer dirty fact to retract the typed one, got %+v (ok=%v)", f, ok)
	}

	// Dirty is permanent.
	c.Observe(numRows(factTypedAfter, "x 7"))
	settle(t, c)
	if f, _ := factFor(c.Pending(), "$2"); f.State != FactDirty {
		t.Errorf("dirty fact recovered: %+v", f)
	}
}

func TestFieldClassifier_ShortRowMakesTypedFieldDirty(t *testing.T) {
	c := NewFieldClassifier()
	defer c.Stop()
	c.Observe(numRows(factTypedAfter, "x 7 8"))
	settle(t, c)
	c.Observe(numRows(1, "x 7"))
	settle(t, c)
	facts := c.Pending()
	if f, _ := factFor(facts, "$2"); f.State != FactTyped {
		t.Errorf("$2 present in the short row, want typed, got %+v", f)
	}
	if f, _ := factFor(facts, "$3"); f.State != FactDirty {
		t.Errorf("$3 missing from the short row, want dirty, got %+v", f)
	}
}

func TestFieldClassifier_FieldFirstSeenLateIsNeverTyped(t *testing.T) {
	c := NewFieldClassifier()
	defer c.Stop()
	c.Observe(numRows(10, "x 7"))
	c.Observe(numRows(factTypedAfter*2, "x 7 8"))
	settle(t, c)
	// The first ten rows had no $3.
	if _, ok := factFor(c.Pending(), "$3"); ok {
		t.Error("$3 was absent from earlier rows and must not type")
	}
	if f, ok := factFor(c.Pending(), "$2"); !ok || f.State != FactTyped {
		t.Errorf("$2 want typed, got %+v", f)
	}
}

func TestFieldClassifier_SourcesDoNotShareFacts(t *testing.T) {
	c := NewFieldClassifier()
	defer c.Stop()
	staging := numRows(1, "x oops")
	staging[0].Env = "staging"
	c.Observe(numRows(factTypedAfter, "x 7"))
	c.Observe(staging)
	settle(t, c)
	f, ok := factFor(c.Pending(), "$2")
	if !ok || f.State != FactTyped || f.Env != "prod" {
		t.Errorf("a dirty row in staging downgraded production: %+v (ok=%v)", f, ok)
	}
}

func TestFieldClassifier_TracksAtMostFactMaxLocators(t *testing.T) {
	c := NewFieldClassifier()
	defer c.Stop()
	var wide []string
	for i := 0; i < factMaxLocators+10; i++ {
		wide = append(wide, "1")
	}
	c.Observe(numRows(factTypedAfter, strings.Join(wide, " ")))
	settle(t, c)
	if got := len(c.Pending()); got != factMaxLocators {
		t.Errorf("tracked %d locators, want the cap %d", got, factMaxLocators)
	}
}

func TestFieldClassifier_FullQueueDisablesRatherThanGuess(t *testing.T) {
	c := &FieldClassifier{
		in:      make(chan []*pb.LogRow), // unbuffered with no reader: always full
		done:    make(chan struct{}),
		sources: map[sourceKey]*factSource{},
	}
	c.Observe(numRows(factTypedAfter, "x 7"))
	if got := c.Pending(); got != nil {
		t.Errorf("a classifier that dropped a batch reported %+v", got)
	}
}

func TestIngest_ClassifiesCommittedRows(t *testing.T) {
	ds, err := NewDuckDBShard("facts", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	if err := ds.Ingest(t.Context(), numRows(factTypedAfter, "x 7"), nil); err != nil {
		t.Fatal(err)
	}
	settle(t, ds.facts)
	if f, ok := factFor(ds.FieldFacts(), "$2"); !ok || f.State != FactTyped {
		t.Errorf("want $2 typed after ingest, got %+v (ok=%v)", f, ok)
	}
	ds.AckFieldFacts(f(ds).Epoch)
	if got := ds.FieldFacts(); len(got) != 0 {
		t.Errorf("acked facts still pending: %+v", got)
	}
}

func f(ds *DuckDBShard) FieldFact { x, _ := factFor(ds.FieldFacts(), "$2"); return x }

func jsonRows(n int, payload string) []*pb.LogRow {
	rows := numRows(n, payload)
	for _, r := range rows {
		r.PayloadJson = []byte(payload)
	}
	return rows
}

func TestFieldClassifier_JQKeyTypedWhenEveryValueIsANumber(t *testing.T) {
	c := NewFieldClassifier()
	defer c.Stop()
	c.Observe(jsonRows(factTypedAfter, `{"q_max_ms":14.2,"count":-3,"shard_id":"a","nested":{"n":1}}`))
	settle(t, c)
	facts := c.Pending()
	for _, loc := range []string{".q_max_ms", ".count"} {
		if f, ok := factFor(facts, loc); !ok || f.State != FactTyped {
			t.Errorf("%s want typed, got %+v (ok=%v)", loc, f, ok)
		}
	}
	for _, loc := range []string{".shard_id", ".nested"} {
		if _, ok := factFor(facts, loc); ok {
			t.Errorf("%s is not a number and must never be reported", loc)
		}
	}
}

// jq compares a string that reads as a number as a string, so only a JSON
// number conforms.
func TestFieldClassifier_JQNonNumbersMakeATypedKeyDirty(t *testing.T) {
	for name, bad := range map[string]string{
		"string that reads as a number": `{"v":"7"}`,
		"null":                          `{"v":null}`,
		"bool":                          `{"v":true}`,
		"array":                         `{"v":[1]}`,
		"object":                        `{"v":{"a":1}}`,
		"key missing":                   `{"w":1}`,
		"payload not an object":         `[1,2]`,
	} {
		t.Run(name, func(t *testing.T) {
			c := NewFieldClassifier()
			defer c.Stop()
			c.Observe(jsonRows(factTypedAfter, `{"v":7}`))
			settle(t, c)
			c.Observe(jsonRows(1, bad))
			settle(t, c)
			if f, ok := factFor(c.Pending(), ".v"); !ok || f.State != FactDirty {
				t.Errorf("want .v retracted after %s, got %+v (ok=%v)", name, f, ok)
			}
		})
	}
}

func TestFieldClassifier_JQKeyFirstSeenLateIsNeverTyped(t *testing.T) {
	c := NewFieldClassifier()
	defer c.Stop()
	c.Observe(jsonRows(10, `{"a":1}`))
	c.Observe(jsonRows(factTypedAfter*2, `{"a":1,"b":2}`))
	settle(t, c)
	if _, ok := factFor(c.Pending(), ".b"); ok {
		t.Error(".b was absent from earlier rows and must not type")
	}
	if f, ok := factFor(c.Pending(), ".a"); !ok || f.State != FactTyped {
		t.Errorf(".a want typed, got %+v", f)
	}
}

// A JSON row's line splits into awk fields too; those junk fields must not
// use up the slots its keys need.
func TestFieldClassifier_JSONRowsAwkFieldsDoNotCrowdOutKeys(t *testing.T) {
	c := NewFieldClassifier()
	defer c.Stop()
	var raw []string
	for i := 0; i < factMaxLocators*2; i++ {
		raw = append(raw, "x")
	}
	rows := jsonRows(factTypedAfter, `{"q":1}`)
	for _, r := range rows {
		r.Raw = strings.Join(raw, " ")
	}
	c.Observe(rows)
	settle(t, c)
	if f, ok := factFor(c.Pending(), ".q"); !ok || f.State != FactTyped {
		t.Errorf(".q want typed, got %+v (ok=%v)", f, ok)
	}
}

func TestFieldClassifier_TracksAtMostFactMaxJQKeys(t *testing.T) {
	c := NewFieldClassifier()
	defer c.Stop()
	var kv []string
	for i := 0; i < factMaxLocators+10; i++ {
		kv = append(kv, fmt.Sprintf(`"k%d":1`, i))
	}
	c.Observe(jsonRows(factTypedAfter, "{"+strings.Join(kv, ",")+"}"))
	settle(t, c)
	if got := len(c.Pending()); got != factMaxLocators {
		t.Errorf("tracked %d keys, want the cap %d", got, factMaxLocators)
	}
}

// Ingest is the only writer of row values, so it is the only place the
// classifier sees them. This fails if another path starts writing fields or
// payload, which would make every fact a guess about rows it never saw.
func TestIngestIsTheOnlyWriterOfRowValues(t *testing.T) {
	writers := regexp.MustCompile("(?is)INSERT\\s+INTO\\s+logs|UPDATE\\s+logs\\s+SET[^`\"]*\\b(fields|payload)\\b|NewAppender\\w*\\([^)]*\"logs\"")
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "worktrees") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if loc := writers.FindIndex(b); loc != nil && filepath.Clean(path) != filepath.Join("shard.go") {
			t.Errorf("%s writes to logs outside DuckDBShard.Ingest: %q", path, fmt.Sprint(string(b[loc[0]:loc[1]])))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// BenchmarkClassifyJSONRow is the classifier's per-row cost on a stats-shaped
// payload (the weft/shard stats source: 15 flat keys, mostly numbers). It runs
// on the classifier goroutine, never the ingest path, but a busy shard feeds
// it every row.
func BenchmarkClassifyJSONRow(b *testing.B) {
	payload := `{"duckdb_bytes":4433653760,"q_count":8,"q_errors":0,"q_max_ms":14.217,"q_mean_ms":7.442625,"q_rows":1224,"shard_id":"shard-20261026T0000-bfed2003","spill_tmp_bytes":0,"total_bytes":4433788666,"ts":"2026-10-30T18:20:50.168227539Z","type":"shard_stats","volume_free_bytes":95269953536,"volume_total_bytes":105089261568,"volume_used_bytes":9819308032,"volume_used_pct":9.343778693930807,"wal_bytes":134906}`
	row := &pb.LogRow{Raw: payload, PayloadJson: []byte(payload), CustomerId: 1, Namespace: "ns", Service: "svc", Env: "prod"}
	c := NewFieldClassifier()
	defer c.Stop()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.mu.Lock()
		c.classify(row)
		c.mu.Unlock()
	}
}

func openAt(t *testing.T, path string) *DuckDBShard {
	t.Helper()
	ds, err := NewDuckDBShard("a", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })
	return ds
}

// mixedRows is n rows of one source with a numeric awk field ($2) and a numeric
// payload key (.q).
func mixedRows(n int, awk, q string) []*pb.LogRow {
	rows := jsonRows(n, `{"q":`+q+`}`)
	for _, r := range rows {
		r.Raw = "x " + awk
	}
	return rows
}

func ingest(t *testing.T, ds *DuckDBShard, rows []*pb.LogRow) {
	t.Helper()
	if err := ds.Ingest(t.Context(), rows, nil); err != nil {
		t.Fatal(err)
	}
	settle(t, ds.facts)
}

func stateRows(t *testing.T, ds *DuckDBShard) int {
	t.Helper()
	var n int
	if err := ds.db.QueryRow(`SELECT COUNT(*) FROM field_facts_state`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// roll is what a handoff does: wait for the classifier, then close.
func roll(t *testing.T, ds *DuckDBShard) {
	t.Helper()
	ds.FlushFieldFacts()
	ds.Close()
}

func TestFieldFacts_RestoredByTheNextInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.duckdb")
	ds := openAt(t, path)
	ingest(t, ds, mixedRows(factTypedAfter, "7", "3"))
	roll(t, ds)

	next := openAt(t, path)
	facts := next.FieldFacts()
	for _, loc := range []string{"$2", ".q"} {
		if f, ok := factFor(facts, loc); !ok || f.State != FactTyped {
			t.Errorf("%s want typed straight after the roll, got %+v (ok=%v)", loc, f, ok)
		}
	}
}

// Results are kept, counts on the way to a result are not: a locator that was
// 600 rows in starts again, so it takes a full 1,000 more.
func TestFieldFacts_ACountOnItsWayRestartsAfterARoll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.duckdb")
	ds := openAt(t, path)
	ingest(t, ds, mixedRows(600, "7", "3"))
	roll(t, ds)

	next := openAt(t, path)
	ingest(t, next, mixedRows(600, "7", "3"))
	if got := next.FieldFacts(); len(got) != 0 {
		t.Fatalf("typed on 600 rows after the roll: %+v", got)
	}
	ingest(t, next, mixedRows(400, "7", "3"))
	if f, ok := factFor(next.FieldFacts(), "$2"); !ok || f.State != FactTyped {
		t.Errorf("want $2 typed after 1,000 rows since the roll, got %+v (ok=%v)", f, ok)
	}
}

// A dirty locator stays dirty after a roll, and a locator the earlier rows
// lacked can't be typed by the rows that follow.
func TestFieldFacts_DirtyAndLateStayOffAfterARoll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.duckdb")
	ds := openAt(t, path)
	ingest(t, ds, append(mixedRows(1, "7abc", `"s"`), mixedRows(10, "7", "3")...))
	roll(t, ds)

	next := openAt(t, path)
	late := mixedRows(factTypedAfter, "7 8", "3") // $3 was absent from the earlier rows
	ingest(t, next, late)
	for _, loc := range []string{"$2", ".q", "$3"} {
		if f, ok := factFor(next.FieldFacts(), loc); ok {
			t.Errorf("%s must not type after the roll, got %+v", loc, f)
		}
	}
}

// A shard with rows and nothing kept (one from before this, or whose state
// was lost) has no facts, and is not rescanned.
func TestFieldFacts_OffForAShardWithRowsAndNoSavedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.duckdb")
	ds := openAt(t, path)
	ingest(t, ds, mixedRows(5, "7", "3"))
	if _, err := ds.db.Exec(`DELETE FROM field_facts_state`); err != nil {
		t.Fatal(err)
	}
	ds.Close()

	next := openAt(t, path)
	ingest(t, next, mixedRows(factTypedAfter*2, "7", "3"))
	if got := next.FieldFacts(); got != nil {
		t.Errorf("reported %+v for rows it never classified", got)
	}
}

// The value is written when a result changes, not when rows are classified.
func TestFieldFacts_WrittenOnlyWhenAResultChanges(t *testing.T) {
	ds := openAt(t, filepath.Join(t.TempDir(), "s.duckdb"))
	stored := func() string {
		t.Helper()
		var s string
		if err := ds.db.QueryRow(`SELECT state FROM field_facts_state WHERE id = 1`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	overwrite := func() {
		t.Helper()
		if _, err := ds.db.Exec(`UPDATE field_facts_state SET state = 'sentinel' WHERE id = 1`); err != nil {
			t.Fatal(err)
		}
	}

	ingest(t, ds, mixedRows(3, "7", "3"))
	if stored() == "" {
		t.Fatal("a new source was not recorded")
	}
	overwrite()
	ingest(t, ds, mixedRows(500, "7", "3")) // more of the same: no result changes
	if got := stored(); got != "sentinel" {
		t.Fatalf("rewritten with no change in any result: %q", got)
	}
	ingest(t, ds, mixedRows(500, "7", "3")) // crosses 1,000: $2 and .q become typed
	if got := stored(); got == "sentinel" {
		t.Error("became typed but was not written")
	}
	overwrite()
	ingest(t, ds, mixedRows(1, "7abc", "3")) // $2 goes dirty
	if got := stored(); got == "sentinel" {
		t.Error("went dirty but was not written")
	}
}

// A classifier that stops (its queue overflowed) misses rows from then on, so
// what was saved no longer accounts for the shard. The next instance must not
// restore it.
func TestFieldFacts_ADisabledClassifierLeavesNothingToRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.duckdb")
	ds := openAt(t, path)
	ingest(t, ds, mixedRows(factTypedAfter, "7", "3")) // typed and saved
	ds.facts.Disable("queue full")
	if err := ds.Ingest(t.Context(), mixedRows(1, "7abc", "3"), nil); err != nil { // lands unclassified
		t.Fatal(err)
	}
	roll(t, ds)

	next := openAt(t, path)
	if got := next.FieldFacts(); got != nil {
		t.Errorf("restored facts that missed a row: %+v", got)
	}
}

// A typed field is one DuckDB's cast and awk's comparison read the same way
// in every row, which the plan relies on to drop the awk stage. They disagree
// on inf, nan and hex floats, so a field holding one is never typed.
func TestFieldClassifier_OnlyFiniteDecimalsTypeAField(t *testing.T) {
	typed := []string{"7", "-3", "+5", "12.5", ".5", "5.", "1e3", "1E-2", "007", "-0", "9007199254740993", "1e308", "4.9e-324"}
	for _, v := range typed {
		c := NewFieldClassifier()
		c.Observe(numRows(factTypedAfter, "x "+v))
		settle(t, c)
		if f, ok := factFor(c.Pending(), "$2"); !ok || f.State != FactTyped {
			t.Errorf("%q: want typed, got %+v (ok=%v)", v, f, ok)
		}
		c.Stop()
	}
	for _, v := range []string{"inf", "-inf", "Inf", "+Inf", "infinity", "nan", "NaN", "-nan", "0x1p3", "0X1P-2", "0x10"} {
		c := NewFieldClassifier()
		c.Observe(numRows(factTypedAfter, "x 7"))
		c.Observe(numRows(1, "x "+v))
		settle(t, c)
		if f, ok := factFor(c.Pending(), "$2"); ok && f.State == FactTyped {
			t.Errorf("%q: a spelling DuckDB reads differently from awk must not leave the field typed", v)
		}
		c.Stop()
	}
	// And a field made only of such spellings never gets typed at all.
	c := NewFieldClassifier()
	defer c.Stop()
	c.Observe(numRows(factTypedAfter, "x inf"))
	settle(t, c)
	if f, ok := factFor(c.Pending(), "$2"); ok && f.State == FactTyped {
		t.Errorf("a field of inf values was typed: %+v", f)
	}
}

// A query's field columns come back as ResultRow.fields in column order, with
// the row's field count, and a missing field is an empty entry.
func TestScanResultRow_FieldForm(t *testing.T) {
	s, err := NewDuckDBShard("fieldform", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows := numRows(1, "a b c d")
	rows = append(rows, numRows(1, "x")[0], numRows(1, "")[0])
	rows[0].TsUnixNs, rows[1].TsUnixNs, rows[2].TsUnixNs = 1e9, 2e9, 3e9
	if err := s.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Query(context.Background(),
		`SELECT CASE WHEN len(fields) = 0 THEN raw ELSE '' END AS raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no, `+
			`list_extract(fields, 1) AS field_1, list_extract(fields, 3) AS field_3, list_extract(fields, -1) AS field_last, len(fields) AS nf `+
			`FROM logs ORDER BY ts`)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		fields []string
		count  int32
		raw    string
	}{
		{[]string{"a", "c", "d"}, 4, ""},
		{[]string{"x", "", "x"}, 1, ""},
		{[]string{"", "", ""}, 0, ""},
	}
	if len(got) != len(want) {
		t.Fatalf("%d rows, want %d", len(got), len(want))
	}
	for i, w := range want {
		r := got[i]
		if r.FieldCount == nil || *r.FieldCount != w.count || !slices.Equal(r.Fields, w.fields) || r.Raw != w.raw {
			t.Errorf("row %d: fields=%q count=%v raw=%q, want fields=%q count=%d raw=%q", i, r.Fields, r.FieldCount, r.Raw, w.fields, w.count, w.raw)
		}
		if len(r.MetaVars) != 0 {
			t.Errorf("row %d: field columns leaked into meta_vars: %v", i, r.MetaVars)
		}
	}
	// A query with no field columns leaves the row in raw form.
	plain, err := s.Query(context.Background(), `SELECT raw, ts FROM logs ORDER BY ts LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	if plain[0].FieldCount != nil || len(plain[0].Fields) != 0 {
		t.Errorf("a plain row has field form: %v %v", plain[0].Fields, plain[0].FieldCount)
	}
}
