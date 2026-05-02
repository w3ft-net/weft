package coordinator

import (
	"bytes"
	"strings"
	"testing"
)

// witHelper sets up a WitFuncs scoped to a fresh customer+user, with
// `n` saves of /bin/scan to populate history. Returns the funcs and
// the user ID for assertions.
func witHelper(t *testing.T, slug string, n int) (*WitFuncs, string) {
	t.Helper()
	db, custID := testStoreAndCustomer(t)
	cust, _ := db.GetCustomerByID(custID)
	_ = cust
	user, err := db.CreateUser("alice-"+slug+"@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserName(user.ID, "Alice"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		src := "echo v" + string(rune('0'+i))
		if err := db.SaveShellFunction(custID, user.ID, "scan", src); err != nil {
			t.Fatal(err)
		}
	}
	return &WitFuncs{Store: db, CustomerID: custID, UserID: user.ID}, user.ID
}

// runWit invokes wf.Run with the given args and returns (stdout,
// stderr, exit-code).
func runWit(t *testing.T, wf *WitFuncs, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := wf.Run(args, nil, &out, &errb)
	return out.String(), errb.String(), code
}

// Subcommand: log -------------------------------------------------

// witMultiActorHelper sets up /bin/scan with three versions saved by
// three distinct users (the same email→user mapping the demo seed
// uses, so the assertion mirrors the demo expectation).
func witMultiActorHelper(t *testing.T) *WitFuncs {
	t.Helper()
	db, custID := testStoreAndCustomer(t)
	alice, err := db.CreateUser("alice@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserName(alice.ID, "Alice"); err != nil {
		t.Fatal(err)
	}
	bob, err := db.CreateUser("bob@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserName(bob.ID, "Bob"); err != nil {
		t.Fatal(err)
	}
	// alice → bob → alice  (same alternation as the multi-customer
	// demo seed, so the assertion catches the same regression).
	saves := []struct {
		userID, src string
	}{
		{alice.ID, "echo v1"},
		{bob.ID, "echo v2"},
		{alice.ID, "echo v3"},
	}
	for _, s := range saves {
		if err := db.SaveShellFunction(custID, s.userID, "scan", s.src); err != nil {
			t.Fatal(err)
		}
	}
	return &WitFuncs{Store: db, CustomerID: custID, UserID: alice.ID}
}

func TestWit_Log_PerVersionAuthor(t *testing.T) {
	wf := witMultiActorHelper(t)
	out, errb, code := runWit(t, wf, "log", "/bin/scan")
	if code != 0 {
		t.Fatalf("wit log: code=%d, stderr=%q", code, errb)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 log entries, got %d:\n%s", len(lines), out)
	}
	// Authorship alternates: HEAD=Alice (v3), HEAD^=Bob (v2),
	// HEAD^2=Alice (v1). The bug being fixed: today every line shows
	// the original creator (Alice), so the per-version actor is lost.
	wantOwners := []string{"Alice", "Bob", "Alice"}
	for i, want := range wantOwners {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d should name %q; got %q", i, want, lines[i])
		}
	}
	// Negative check: HEAD^ must not still say "Alice", which is the
	// most common failure mode of the displayed-from-created_by bug.
	if strings.Contains(lines[1], "Alice") {
		t.Errorf("HEAD^ should be Bob, not Alice; got %q", lines[1])
	}
}

func TestWit_Log_NewestFirstWithHashAndOwner(t *testing.T) {
	wf, _ := witHelper(t, "log-shape", 3)
	out, errb, code := runWit(t, wf, "log", "/bin/scan")
	if code != 0 {
		t.Fatalf("wit log: code=%d, stderr=%q", code, errb)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 log entries, got %d:\n%s", len(lines), out)
	}
	// First line contains "HEAD" and the owner Alice; last line is
	// the oldest (HEAD^2).
	if !strings.Contains(lines[0], "HEAD") || !strings.Contains(lines[0], "Alice") {
		t.Errorf("first log line should mention HEAD and Alice; got %q", lines[0])
	}
	if !strings.Contains(lines[2], "HEAD^2") {
		t.Errorf("third log line should be HEAD^2; got %q", lines[2])
	}
	// Each line carries a 7-char hex short hash (the `*` HEAD marker
	// or `  ` indent precedes it).
	for i, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Errorf("line %d: too few fields: %q", i, line)
			continue
		}
		// First field is "*" or the hash. If "*", hash is fields[1].
		hashCand := fields[0]
		if fields[0] == "*" {
			hashCand = fields[1]
		}
		if !isShortHash(hashCand) {
			t.Errorf("line %d: expected 7-char hex hash, got %q in %q", i, hashCand, line)
		}
	}
}

func TestWit_Log_DashP_IncludesDiffsBetweenVersions(t *testing.T) {
	wf, _ := witHelper(t, "log-p", 3)
	out, _, code := runWit(t, wf, "log", "-p", "/bin/scan")
	if code != 0 {
		t.Fatalf("wit log -p: code=%d", code)
	}
	// Diff sections appear between consecutive entries.
	if !strings.Contains(out, "diff ") {
		t.Errorf("wit log -p should include 'diff' section: %q", out)
	}
	// Unified-style markers.
	if !strings.Contains(out, "- echo v2") {
		t.Errorf("expected '- echo v2' in diff output: %q", out)
	}
	if !strings.Contains(out, "+ echo v3") {
		t.Errorf("expected '+ echo v3' in diff output: %q", out)
	}
}

// Subcommand: show ------------------------------------------------

func TestWit_Show_HEADCarat(t *testing.T) {
	wf, _ := witHelper(t, "show-caret", 3)
	out, _, code := runWit(t, wf, "show", "/bin/scan", "HEAD^")
	if code != 0 {
		t.Fatalf("show: code=%d", code)
	}
	if strings.TrimRight(out, "\n") != "echo v2" {
		t.Errorf("show HEAD^: got %q, want %q", out, "echo v2\n")
	}
}

func TestWit_Show_HashPrefix(t *testing.T) {
	wf, _ := witHelper(t, "show-hash", 2)
	// Resolve current HEAD's short hash.
	versions, _ := wf.Store.LogShellFunction(wf.CustomerID, "scan")
	short := versions[0].ContentHash[:7]

	out, errb, code := runWit(t, wf, "show", "/bin/scan", short)
	if code != 0 {
		t.Fatalf("show by hash: code=%d, stderr=%q", code, errb)
	}
	if strings.TrimRight(out, "\n") != "echo v2" {
		t.Errorf("show by hash: got %q, want echo v2", out)
	}
}

// Subcommand: diff ------------------------------------------------

func TestWit_Diff_Range(t *testing.T) {
	wf, _ := witHelper(t, "diff-range", 3)
	out, _, code := runWit(t, wf, "diff", "/bin/scan", "HEAD^2..HEAD")
	if code != 0 {
		t.Fatalf("diff: code=%d", code)
	}
	// Header: --- a/scan (... ) and +++ b/scan (...).
	if !strings.Contains(out, "--- a/scan") || !strings.Contains(out, "+++ b/scan") {
		t.Errorf("diff missing unified headers: %q", out)
	}
	if !strings.Contains(out, "- echo v1") || !strings.Contains(out, "+ echo v3") {
		t.Errorf("diff missing change markers: %q", out)
	}
}

// Subcommand: checkout --------------------------------------------

func TestWit_Checkout_AdvancesHEAD(t *testing.T) {
	wf, _ := witHelper(t, "checkout", 3)
	if _, _, code := runWit(t, wf, "checkout", "/bin/scan", "HEAD^2"); code != 0 {
		t.Fatalf("checkout: code=%d", code)
	}
	fn, _ := wf.Store.GetShellFunction(wf.CustomerID, "scan")
	if fn.Source != "echo v1" {
		t.Errorf("after checkout: live source = %q, want echo v1", fn.Source)
	}
}

// Subcommand: amend -----------------------------------------------

func TestWit_Amend_HEAD_M_Flag(t *testing.T) {
	wf, _ := witHelper(t, "amend-m", 2)
	if _, _, code := runWit(t, wf, "amend", "-m", "fix the awk filter", "/bin/scan"); code != 0 {
		t.Fatalf("amend: code=%d", code)
	}
	v, _ := wf.Store.ResolveShellFunctionRef(wf.CustomerID, "scan", "HEAD")
	if v.Message != "fix the awk filter" {
		t.Errorf("HEAD message after amend: got %q, want %q", v.Message, "fix the awk filter")
	}
}

func TestWit_Amend_HistoryRef(t *testing.T) {
	wf, _ := witHelper(t, "amend-history", 3)
	// HEAD^2 = oldest history row.
	if _, _, code := runWit(t, wf, "amend", "-m", "first version", "/bin/scan", "HEAD^2"); code != 0 {
		t.Fatalf("amend HEAD^2: code=%d", code)
	}
	v, _ := wf.Store.ResolveShellFunctionRef(wf.CustomerID, "scan", "HEAD^2")
	if v.Message != "first version" {
		t.Errorf("HEAD^2 message: got %q, want first version", v.Message)
	}
	// HEAD message unchanged.
	head, _ := wf.Store.ResolveShellFunctionRef(wf.CustomerID, "scan", "HEAD")
	if head.Message != "" {
		t.Errorf("HEAD message changed unexpectedly: %q", head.Message)
	}
}

// Errors: file-less subcommands -----------------------------------

func TestWit_FilelessSubcommandsError(t *testing.T) {
	wf, _ := witHelper(t, "fileless", 1)
	for _, sub := range []string{"log", "show", "diff", "checkout", "amend"} {
		var args []string
		switch sub {
		case "amend":
			args = []string{sub, "-m", "msg"}
		case "show", "checkout":
			args = []string{sub} // also missing ref, but we test path-missing first
		default:
			args = []string{sub}
		}
		_, errb, code := runWit(t, wf, args...)
		if code == 0 {
			t.Errorf("wit %s without path: expected non-zero exit, got 0", sub)
		}
		if !strings.Contains(strings.ToLower(errb), "usage") &&
			!strings.Contains(strings.ToLower(errb), "path") {
			t.Errorf("wit %s without path: stderr should mention path/usage; got %q", sub, errb)
		}
	}
}

func TestWit_NonHistoryNamespaceErrors(t *testing.T) {
	wf, _ := witHelper(t, "ns-error", 1)
	for _, path := range []string{"/tmp/foo", "/etc/usage", "/log/api.log"} {
		_, errb, code := runWit(t, wf, "log", path)
		if code == 0 {
			t.Errorf("wit log %s: expected non-zero exit", path)
		}
		if !strings.Contains(strings.ToLower(errb), "history not tracked") {
			t.Errorf("wit log %s: stderr should mention 'history not tracked'; got %q", path, errb)
		}
	}
}

// Subcommand routing ----------------------------------------------

func TestWit_UnknownSubcommandErrors(t *testing.T) {
	wf, _ := witHelper(t, "unknown", 1)
	_, errb, code := runWit(t, wf, "frobnicate", "/bin/scan")
	if code == 0 {
		t.Errorf("wit frobnicate: expected non-zero exit")
	}
	if !strings.Contains(errb, "unknown subcommand") {
		t.Errorf("expected 'unknown subcommand' in stderr; got %q", errb)
	}
}

func TestWit_NoSubcommandErrors(t *testing.T) {
	wf, _ := witHelper(t, "nosub", 1)
	_, errb, code := runWit(t, wf)
	if code == 0 {
		t.Errorf("wit (no args): expected non-zero exit")
	}
	if !strings.Contains(strings.ToLower(errb), "usage") {
		t.Errorf("expected 'usage' in stderr; got %q", errb)
	}
}

// helpers ---------------------------------------------------------

func isShortHash(s string) bool {
	if len(s) != 7 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}
