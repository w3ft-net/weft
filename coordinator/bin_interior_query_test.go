package coordinator

import (
	"strings"
	"testing"
	"time"

	"github.com/kevlarcon/weft/compute"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

// interiorQueryFixture spins up a coordinator with:
//   - a tenant-scoped Store (postgres via testStoreAndCustomer)
//   - an in-memory DuckDB shard populated with rows for apps "api"
//     and "web", scoped to the test customer
//   - a local compute pool for compute stages
//
// Returns the coordinator, the test Store, the customer name (used
// for Query's `customer` argument), and the customer ID.
//
// The shard has:
//   - 2 ERROR rows for app=api, 1 INFO row for app=api
//   - 1 ERROR row for app=web, 1 WARN row for app=web
func newInteriorQueryFixture(t *testing.T) (*Coordinator, *store.Store, string, uint) {
	t.Helper()
	db, custID := testStoreAndCustomer(t)

	cust, err := db.GetCustomerByID(custID)
	if err != nil {
		t.Fatalf("get customer: %v", err)
	}

	// Allowlist entries — without these, executeQuery rejects the
	// source-app routing with "unknown source".
	if _, err := db.FindOrCreateApp(custID, "api"); err != nil {
		t.Fatalf("create api namespace: %v", err)
	}
	if _, err := db.FindOrCreateApp(custID, "web"); err != nil {
		t.Fatalf("create web namespace: %v", err)
	}

	s, err := shard.NewDuckDBShard("interior-test", "")
	if err != nil {
		t.Fatalf("new shard: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	now := time.Now()
	rows := []*pb.LogRow{
		logRow(1, "api", "ERROR", cust.Name, now.Add(-10*time.Minute)),
		logRow(2, "api", "ERROR", cust.Name, now.Add(-8*time.Minute)),
		logRow(3, "api", "INFO", cust.Name, now.Add(-6*time.Minute)),
		logRow(4, "web", "ERROR", cust.Name, now.Add(-5*time.Minute)),
		logRow(5, "web", "WARN", cust.Name, now.Add(-2*time.Minute)),
	}
	if err := s.Ingest(rows); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	c := NewCoordinator(CoordinatorConfig{
		Pool:  &compute.LocalComputePool{Parallelism: 1},
		Store: db,
	})
	c.Registry.Add(&ShardEntry{
		ID:        s.ID(),
		Apps:      []string{"api", "web"},
		State:     StateIngesting,
		Shard:     s,
		CreatedAt: time.Now(),
	})

	return c, db, cust.Name, custID
}

func logRow(lineNum int64, app, level, customer string, ts time.Time) *pb.LogRow {
	return &pb.LogRow{
		LineNum:  lineNum,
		Raw:      level + " " + app + " line",
		Fields:   []string{level, app, "line"},
		TsUnixNs: ts.UnixNano(),
		Level:    level,
		App:      app,
		Instance: "i-1",
		Customer: customer,
	}
}

func lastHourRange() TimeRange {
	now := time.Now()
	return TimeRange{Start: now.Add(-time.Hour), End: now.Add(time.Minute)}
}

// The motivating example from the plan: an echo preamble, a query
// whose predicate must push down to SQL, an echo postamble. Before
// the interceptor, only the echos fired — the query ran through
// native bash with an empty /log pseudofile read, producing "0". With
// the interceptor, the query reaches the shard and returns the real
// count.
func TestInteriorQuery_EchoBeforeAndAfter(t *testing.T) {
	c, db, custName, custID := newInteriorQueryFixture(t)

	body := `echo starting
cat /log/api.log | grep ERROR | wc -l
echo finished`
	if err := db.SaveShellFunction(custID, "", "scan", body); err != nil {
		t.Fatalf("save script: %v", err)
	}

	result, err := c.Query("scan", lastHourRange(), custName, "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(result.Text), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 output lines, got %d: %q (stderr=%q)", len(lines), result.Text, result.Stderr)
	}
	if lines[0] != "starting" {
		t.Errorf("line 0: want `starting`, got %q", lines[0])
	}
	if lines[1] != "2" {
		t.Errorf("line 1 (ERROR count): want `2`, got %q — interceptor likely not firing", lines[1])
	}
	if lines[2] != "finished" {
		t.Errorf("line 2: want `finished`, got %q", lines[2])
	}
}

// Positional arguments must bind per invocation. The interceptor sees
// the fully-expanded pipeline (cat /log/api.log, not cat /log/$1).
func TestInteriorQuery_PositionalArg(t *testing.T) {
	c, db, custName, custID := newInteriorQueryFixture(t)

	body := `echo starting
cat /log/$1.log | grep ERROR | wc -l`
	if err := db.SaveShellFunction(custID, "", "scan", body); err != nil {
		t.Fatalf("save script: %v", err)
	}

	// api has 2 ERROR rows, web has 1.
	cases := []struct {
		arg  string
		want string
	}{
		{"api", "2"},
		{"web", "1"},
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			result, err := c.Query("scan "+tc.arg, lastHourRange(), custName, "")
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			lines := strings.Split(strings.TrimSpace(result.Text), "\n")
			if len(lines) < 2 {
				t.Fatalf("want >=2 lines, got %q (stderr=%q)", result.Text, result.Stderr)
			}
			if lines[1] != tc.want {
				t.Errorf("arg=%s: ERROR count = %q, want %q", tc.arg, lines[1], tc.want)
			}
		})
	}
}

// For-loop iteration is the case that kills static AST pre-expansion:
// $app can't be resolved until the loop runs, which is why the hook
// is runtime. The interceptor must see one dispatch per iteration
// with each iteration's value substituted.
func TestInteriorQuery_InsideForLoop(t *testing.T) {
	c, db, custName, custID := newInteriorQueryFixture(t)

	body := `for app in api web; do
  cat /log/$app.log | grep ERROR | wc -l
done`
	if err := db.SaveShellFunction(custID, "", "loop", body); err != nil {
		t.Fatalf("save script: %v", err)
	}

	result, err := c.Query("loop", lastHourRange(), custName, "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(result.Text), "\n")
	// Two iterations: api has 2 ERRORs, web has 1.
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (one per iteration), got %d: %q", len(lines), result.Text)
	}
	if lines[0] != "2" {
		t.Errorf("iter 0 (api): want 2, got %q", lines[0])
	}
	if lines[1] != "1" {
		t.Errorf("iter 1 (web): want 1, got %q", lines[1])
	}
}

// Command substitution: the inner pipeline is query-shaped, runs
// through the interceptor, and its stdout lands in the variable
// assignment. The outer echo then produces the stitched line.
func TestInteriorQuery_CommandSubstitution(t *testing.T) {
	c, db, custName, custID := newInteriorQueryFixture(t)

	body := `count=$(cat /log/api.log | grep ERROR | wc -l)
echo "found $count"`
	if err := db.SaveShellFunction(custID, "", "count_errs", body); err != nil {
		t.Fatalf("save script: %v", err)
	}

	result, err := c.Query("count_errs", lastHourRange(), custName, "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	got := strings.TrimSpace(result.Text)
	if got != "found 2" {
		t.Errorf("stdout = %q, want `found 2`", got)
	}
}

// && short-circuits on exit status. A successful interceptor call
// returns exit 0, so the RHS runs. This case also happens to exercise
// a body where the interior query has && in the first list and a
// trailing echo in the second — the splicer bails (multi-list body),
// the interceptor handles the query, warp handles the &&.
func TestInteriorQuery_AndShortCircuit(t *testing.T) {
	c, db, custName, custID := newInteriorQueryFixture(t)

	body := `cat /log/api.log | grep ERROR | wc -l && echo ok`
	if err := db.SaveShellFunction(custID, "", "and_ok", body); err != nil {
		t.Fatalf("save script: %v", err)
	}

	result, err := c.Query("and_ok", lastHourRange(), custName, "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(result.Text), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (count + ok), got %d: %q", len(lines), result.Text)
	}
	if lines[0] != "2" {
		t.Errorf("line 0: want 2, got %q", lines[0])
	}
	if lines[1] != "ok" {
		t.Errorf("line 1: want ok, got %q", lines[1])
	}
}

// Function bodies host interceptor dispatch too. This is the
// "assign query output to a variable inside a bash function and
// conditionally return" case: the cmdsub inside the function captures
// the interior query's stdout into a local, the function decides
// based on the count, and the caller sees the function's stdout. The
// interceptor has no special awareness of function scope — warp's
// callFunction simply invokes execCommand on the body, which walks
// into execPipeline for each interior pipeline, which fires the hook
// as usual.
func TestInteriorQuery_FunctionWithConditionalReturn(t *testing.T) {
	c, db, custName, custID := newInteriorQueryFixture(t)

	// api has 2 ERROR rows (above the threshold of 1 → returns non-zero
	// with "high" branch). web has 1 ERROR row (at threshold → returns
	// 0 with "ok" branch). Exercises both return paths.
	body := `error_check() {
  local count=$(cat /log/$1.log | grep ERROR | wc -l)
  if [ "$count" -gt 1 ]; then
    echo "$1: high ($count)"
    return 1
  fi
  echo "$1: ok ($count)"
  return 0
}
error_check api
error_check web`
	if err := db.SaveShellFunction(custID, "", "check_all", body); err != nil {
		t.Fatalf("save script: %v", err)
	}

	result, err := c.Query("check_all", lastHourRange(), custName, "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(result.Text), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (one per error_check call), got %d: %q (stderr=%q)", len(lines), result.Text, result.Stderr)
	}
	if lines[0] != "api: high (2)" {
		t.Errorf("api branch: got %q, want `api: high (2)`", lines[0])
	}
	if lines[1] != "web: ok (1)" {
		t.Errorf("web branch: got %q, want `web: ok (1)`", lines[1])
	}
}

// Same fundamental shape — function + cmdsub + interceptor — but
// with the function being defined in ONE /bin script and called
// from another. Proves the function registration flows through the
// interpreter across /bin boundaries (registered when the first
// script runs, visible to the second because execScript doesn't
// reset interp.functions).
//
// Scoping note: in bash semantics each script is normally its own
// environment. Here both scripts share an interpreter because
// runLocal drives a single warp instance per request, and /bin
// invocations recurse through execSimple's PATH branch (see exec.go
// under "PATH resolution") without isolating state. That's an
// intentional convenience — users can factor helpers across /bin
// files without ceremony — but worth pinning with a test so a
// future scoping change here can't silently land.
func TestInteriorQuery_FunctionDefinedInSeparateBinScript(t *testing.T) {
	c, db, custName, custID := newInteriorQueryFixture(t)

	// /bin/lib: defines error_count but doesn't call it.
	lib := `error_count() {
  cat /log/$1.log | grep ERROR | wc -l
}`
	if err := db.SaveShellFunction(custID, "", "lib", lib); err != nil {
		t.Fatalf("save lib: %v", err)
	}

	// /bin/report: sources the lib (runs it, registering the function),
	// then calls error_count. The lib "runs" as a function-definition
	// script — its body is `error_count() { ... }`, which when
	// executed registers the function on the current interpreter and
	// returns without output.
	report := `lib
echo api: $(error_count api)
echo web: $(error_count web)`
	if err := db.SaveShellFunction(custID, "", "report", report); err != nil {
		t.Fatalf("save report: %v", err)
	}

	result, err := c.Query("report", lastHourRange(), custName, "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(result.Text), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q (stderr=%q)", len(lines), result.Text, result.Stderr)
	}
	if lines[0] != "api: 2" {
		t.Errorf("line 0: got %q, want `api: 2`", lines[0])
	}
	if lines[1] != "web: 1" {
		t.Errorf("line 1: got %q, want `web: 1`", lines[1])
	}
}

// Sanity: single-pipeline bodies still take the pre-splice path and
// work end-to-end. This guards against regressions in the splicer
// from the interceptor work.
func TestInteriorQuery_SinglePipelineStillSplices(t *testing.T) {
	c, db, custName, custID := newInteriorQueryFixture(t)

	body := `cat /log/api.log | grep ERROR | wc -l`
	if err := db.SaveShellFunction(custID, "", "errs", body); err != nil {
		t.Fatalf("save script: %v", err)
	}

	result, err := c.Query("errs api", lastHourRange(), custName, "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := strings.TrimSpace(result.Text); got != "2" {
		t.Errorf("stdout = %q, want 2", got)
	}
}
