package shard

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

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

func waitScanned(t *testing.T, c *FieldClassifier) {
	t.Helper()
	for i := 0; i < 5000; i++ {
		c.mu.Lock()
		ready, disabled := c.ready, c.disabled
		c.mu.Unlock()
		if disabled {
			t.Fatal("classifier disabled during the scan")
		}
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("scan never finished")
}

// reopen ingests first into a shard on disk, closes it, and opens it again.
func reopen(t *testing.T, first []*pb.LogRow) *DuckDBShard {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.duckdb")
	ds, err := NewDuckDBShard("a", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ds.Ingest(t.Context(), first, nil); err != nil {
		t.Fatal(err)
	}
	ds.Close()
	ds, err = NewDuckDBShard("a", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })
	return ds
}

func TestReopenedShardScansItsRowsBeforeReportingFacts(t *testing.T) {
	ds := reopen(t, numRows(factTypedAfter, "x 7"))
	waitScanned(t, ds.facts)
	if f, ok := factFor(ds.FieldFacts(), "$2"); !ok || f.State != FactTyped {
		t.Errorf("scan of %d numeric rows: want $2 typed, got %+v (ok=%v)", factTypedAfter, f, ok)
	}
}

func TestReopenedShardScanSeesTheViolationsAlreadyOnDisk(t *testing.T) {
	rows := append(numRows(factTypedAfter, "x 7"), numRows(1, "x 7abc")...)
	ds := reopen(t, rows)
	waitScanned(t, ds.facts)
	// Plenty of new numeric rows must not make up for the stored one.
	if err := ds.Ingest(t.Context(), numRows(factTypedAfter, "x 7"), nil); err != nil {
		t.Fatal(err)
	}
	settle(t, ds.facts)
	if f, ok := factFor(ds.FieldFacts(), "$2"); ok {
		t.Errorf("$2 held 7abc on disk but reported %+v", f)
	}
}

func TestReopenedShardReportsNothingWhileScanning(t *testing.T) {
	c := NewFieldClassifier()
	defer c.Stop()
	c.BeginScan()
	c.Observe(numRows(factTypedAfter, "x 7"))
	settle(t, c)
	if got := c.Pending(); got != nil {
		t.Errorf("reported %+v before the scan ended", got)
	}
	c.EndScan()
	if _, ok := factFor(c.Pending(), "$2"); !ok {
		t.Error("nothing reported after the scan ended")
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
