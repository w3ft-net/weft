package shellfs

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/graybeardcorp/warp/pkg/bash"

	"github.com/graybeardcorp/weft/mounts"
)

// runShell executes a bash script against a ShellFS-backed interpreter
// and returns stdout and stderr. The goal of these tests is to exercise
// the whole pipe: warp parses `>` / `>>` / `mkdir`, invokes the FS
// methods on ShellFS, which round-trip through the store.
func runShell(t *testing.T, fs *ShellFS, script string) (string, string) {
	t.Helper()
	interp := bash.NewInterpreter()
	interp.SetFS(fs)
	var out, errb bytes.Buffer
	interp.SetStdout(&out)
	interp.SetStderr(&errb)
	ast, err := bash.Parse(script)
	if err != nil {
		t.Fatalf("parse %q: %v", script, err)
	}
	interp.ExecScript(ast)
	return out.String(), errb.String()
}

func TestTmp_WriteReadThroughShell(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-rw")
	fs := New(db, cust.ID, nil, WithTier("swatch"))

	_, errOut := runShell(t, fs, `echo hello > /tmp/greeting`)
	if errOut != "" {
		t.Fatalf("write failed: %s", errOut)
	}

	out, _ := runShell(t, fs, `cat /tmp/greeting`)
	if strings.TrimRight(out, "\n") != "hello" {
		t.Errorf("cat: got %q, want %q", out, "hello\n")
	}
}

func TestTmp_AppendViaDoubleRedirect(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-ap")
	fs := New(db, cust.ID, nil, WithTier("swatch"))

	runShell(t, fs, `echo first > /tmp/log`)
	runShell(t, fs, `echo second >> /tmp/log`)
	runShell(t, fs, `echo third  >> /tmp/log`)

	out, _ := runShell(t, fs, `cat /tmp/log`)
	want := "first\nsecond\nthird\n"
	if out != want {
		t.Errorf("cat after appends: got %q, want %q", out, want)
	}
}

func TestTmp_MkdirAndNestedWrite(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-mk")
	fs := New(db, cust.ID, nil, WithTier("swatch"))

	runShell(t, fs, `mkdir /tmp/inc`)
	runShell(t, fs, `echo trace > /tmp/inc/details`)

	out, _ := runShell(t, fs, `cat /tmp/inc/details`)
	if strings.TrimRight(out, "\n") != "trace" {
		t.Errorf("nested cat: got %q, want trace", out)
	}

	// ls /tmp should show the directory.
	out, _ = runShell(t, fs, `ls /tmp`)
	if !strings.Contains(out, "inc") {
		t.Errorf("ls /tmp: got %q, want it to contain 'inc'", out)
	}
}

func TestTmp_RmRfSubtree(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-rm")
	fs := New(db, cust.ID, nil, WithTier("swatch"))

	runShell(t, fs, `mkdir /tmp/cleanup`)
	runShell(t, fs, `echo a > /tmp/cleanup/a`)
	runShell(t, fs, `echo b > /tmp/cleanup/b`)
	runShell(t, fs, `rm -rf /tmp/cleanup`)

	out, errOut := runShell(t, fs, `cat /tmp/cleanup/a`)
	if out != "" {
		t.Errorf("expected empty stdout after rm -rf, got %q", out)
	}
	if !strings.Contains(strings.ToLower(errOut), "no such") {
		t.Errorf("expected 'no such' error after rm -rf, got %q", errOut)
	}
}

func TestTmp_PipelineRedirectIntoFile(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-pipe")
	fs := New(db, cust.ID, nil, WithTier("swatch"))

	// Simulate a filter-save pattern: prepare a file, read it, filter,
	// redirect to /tmp. This is the shape the plan promises:
	// `cat /log/api.log | grep ERR > /tmp/errs`.
	runShell(t, fs, `echo "INFO ok"    > /tmp/source`)
	runShell(t, fs, `echo "ERROR boom" >> /tmp/source`)
	runShell(t, fs, `echo "INFO fine"  >> /tmp/source`)
	runShell(t, fs, `echo "ERROR fail" >> /tmp/source`)

	_, errOut := runShell(t, fs, `cat /tmp/source | grep ERROR > /tmp/errs`)
	if errOut != "" {
		t.Fatalf("pipeline write failed: %s", errOut)
	}
	out, _ := runShell(t, fs, `cat /tmp/errs`)
	want := "ERROR boom\nERROR fail\n"
	if out != want {
		t.Errorf("filtered contents: got %q, want %q", out, want)
	}
}

func TestTmp_FilesAreNotExecutable(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-nox")
	fs := New(db, cust.ID, nil, WithTier("swatch"))

	// Put something that LOOKS like a script in /tmp.
	runShell(t, fs, `echo "echo pwned" > /tmp/evil`)
	// Try to run it as a bare command. Warp's resolver only searches
	// /bin/, so this should fail without "pwned" ever appearing.
	out, errOut := runShell(t, fs, `evil`)
	if strings.Contains(out, "pwned") {
		t.Errorf("/tmp file should not be executable, got stdout %q", out)
	}
	if errOut == "" {
		t.Errorf("expected command-not-found error, got empty stderr")
	}
}

func TestTmp_LsLongShowsTmpFiles(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-lsl")
	fs := New(db, cust.ID, nil, WithTier("swatch"))

	runShell(t, fs, `echo data > /tmp/report`)
	out, _ := runShell(t, fs, `ls -l /tmp`)
	if !strings.Contains(out, "report") {
		t.Errorf("ls -l /tmp should list 'report', got %q", out)
	}
	// Size should appear (the stored byte_size for "data\n" is 5).
	if !strings.Contains(out, "5") {
		t.Errorf("ls -l /tmp should show size 5, got %q", out)
	}
	// ModTime should render as YYYY-MM-DD...
	if !strings.Contains(out, "20") {
		t.Errorf("ls -l /tmp should render modtime year, got %q", out)
	}
}

func TestTmp_CpThroughShell(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-cp")
	fs := New(db, cust.ID, nil, WithTier("swatch"))

	runShell(t, fs, `echo source-data > /tmp/src`)
	_, errOut := runShell(t, fs, `cp /tmp/src /tmp/dst`)
	if errOut != "" {
		t.Fatalf("cp: %s", errOut)
	}
	out, _ := runShell(t, fs, `cat /tmp/dst`)
	if strings.TrimRight(out, "\n") != "source-data" {
		t.Errorf("dst contents: got %q", out)
	}
}

func TestTmp_MvThroughShell(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-mv")
	fs := New(db, cust.ID, nil, WithTier("swatch"))

	runShell(t, fs, `echo moveme > /tmp/old`)
	runShell(t, fs, `mv /tmp/old /tmp/new`)

	out, errOut := runShell(t, fs, `cat /tmp/old`)
	if out != "" {
		t.Errorf("expected /tmp/old gone, got %q", out)
	}
	if !strings.Contains(strings.ToLower(errOut), "no such") {
		t.Errorf("expected 'no such' error reading /tmp/old, got %q", errOut)
	}
	out, _ = runShell(t, fs, `cat /tmp/new`)
	if strings.TrimRight(out, "\n") != "moveme" {
		t.Errorf("/tmp/new contents: got %q", out)
	}
}

func TestTmp_TouchThroughShell(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-touch")
	fs := New(db, cust.ID, nil, WithTier("swatch"))

	// touch on a missing path creates an empty file.
	_, errOut := runShell(t, fs, `touch /tmp/empty`)
	if errOut != "" {
		t.Fatalf("touch: %s", errOut)
	}
	out, _ := runShell(t, fs, `cat /tmp/empty`)
	if out != "" {
		t.Errorf("touched empty file: got %q, want empty", out)
	}
	out, _ = runShell(t, fs, `ls /tmp`)
	if !strings.Contains(out, "empty") {
		t.Errorf("ls /tmp should show 'empty', got %q", out)
	}
}

// cp into /bin goes through the same save path as the redirect.
func TestTmp_CpCrossMountWrites(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-xns")
	fs := New(db, cust.ID, nil, WithTier("swatch"),
		WithUser("test-owner", ownerPerms()))

	runShell(t, fs, `echo notascript > /tmp/x`)
	_, errOut := runShell(t, fs, `cp /tmp/x /bin/y`)
	if errOut != "" {
		t.Fatalf("cp into /bin: %s", errOut)
	}
	if out, _ := runShell(t, fs, `cat /bin/y`); !strings.Contains(out, "notascript") {
		t.Errorf("cp /tmp/x /bin/y should write /bin/y; got %q", out)
	}
	runShell(t, fs, `rm /bin/y`)
}

// The next group exercises the multi-file analysis builtins against
// real /tmp files. These tests are the "we read from Postgres"
// proof-points for diff/comm/join/paste/wc — the warp-level tests
// for those commands use MemFS, so they prove the algorithms but
// don't touch the store. If someone broke ShellFS.ReadFile for /tmp
// or the SQL behind it, the tests below would catch it; the
// MemFS-based tests would not.

func TestTmp_DiffOverPostgres(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-diff")
	fs := New(db, cust.ID, nil, WithTier("swatch"))

	runShell(t, fs, `echo old-value > /tmp/a`)
	runShell(t, fs, `echo new-value > /tmp/b`)
	out, _ := runShell(t, fs, `diff /tmp/a /tmp/b`)
	if !strings.Contains(out, "- old-value") {
		t.Errorf("diff should show removed line: %q", out)
	}
	if !strings.Contains(out, "+ new-value") {
		t.Errorf("diff should show added line: %q", out)
	}
}

func TestTmp_CommOverPostgres(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-comm")
	fs := New(db, cust.ID, nil, WithTier("swatch"))

	// Both sorted. Lines unique to A, unique to B, shared.
	runShell(t, fs, `printf 'alpha\nbeta\ngamma\n' > /tmp/a`)
	runShell(t, fs, `printf 'beta\ndelta\ngamma\n' > /tmp/b`)
	out, _ := runShell(t, fs, `comm /tmp/a /tmp/b`)
	if !strings.Contains(out, "alpha\n") {
		t.Errorf("expected 'alpha' as col-1 (only in A): %q", out)
	}
	if !strings.Contains(out, "\tdelta\n") {
		t.Errorf("expected 'delta' as col-2 (only in B): %q", out)
	}
	if !strings.Contains(out, "\t\tgamma\n") {
		t.Errorf("expected 'gamma' as col-3 (in both): %q", out)
	}
}

func TestTmp_JoinOverPostgres(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-join")
	fs := New(db, cust.ID, nil, WithTier("swatch"))

	runShell(t, fs, `printf '1 widget\n2 sprocket\n' > /tmp/orders`)
	runShell(t, fs, `printf '1 10\n2 20\n' > /tmp/prices`)
	out, _ := runShell(t, fs, `join /tmp/orders /tmp/prices`)
	if !strings.Contains(out, "1 widget 10") || !strings.Contains(out, "2 sprocket 20") {
		t.Errorf("join: missing expected rows in %q", out)
	}
}

func TestTmp_PasteOverPostgres(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-paste")
	fs := New(db, cust.ID, nil, WithTier("swatch"))

	runShell(t, fs, `printf '1\n2\n3\n' > /tmp/nums`)
	runShell(t, fs, `printf 'a\nb\nc\n' > /tmp/letters`)
	out, _ := runShell(t, fs, `paste -d , /tmp/nums /tmp/letters`)
	want := "1,a\n2,b\n3,c\n"
	if out != want {
		t.Errorf("paste: got %q, want %q", out, want)
	}
}

func TestTmp_WcOverPostgres(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-wc")
	fs := New(db, cust.ID, nil, WithTier("swatch"))

	runShell(t, fs, `printf 'one\ntwo\nthree\n' > /tmp/x`)
	out, _ := runShell(t, fs, `wc -l /tmp/x`)
	if !strings.Contains(out, "3") || !strings.Contains(out, "/tmp/x") {
		t.Errorf("wc -l /tmp/x: got %q, want '3 /tmp/x'", out)
	}

	// -L (longest line) reaches into the stored content, not just metadata.
	runShell(t, fs, `printf 'short\nmuch longer line\nmid\n' > /tmp/y`)
	out, _ = runShell(t, fs, `wc -L /tmp/y`)
	if !strings.Contains(out, "16") {
		t.Errorf("wc -L /tmp/y: got %q, want '16 /tmp/y'", out)
	}

	// Multi-file: per-file rows + a 'total' summary.
	out, _ = runShell(t, fs, `wc -l /tmp/x /tmp/y`)
	if !strings.Contains(out, "total") {
		t.Errorf("multi-file wc should include 'total' row: %q", out)
	}
}

// TestTmp_FirstWriteShowsExpiryNotice covers the courtesy warning
// printed the first time anyone on a customer account writes to /tmp.
// Three things to assert:
//
//  1. The warning shows up on the side channel after the first write.
//  2. It does NOT show up on the next write within the same session.
//  3. It does NOT show up on a fresh ShellFS for the same customer
//     (the persistent flag survives session boundaries).
//
// TestTmp_WriteRecordsCreatorViaShell exercises the userID path
// from ShellFS construction (WithUser) → shell write → store row.
// If anything in that chain drops the userID, the row's
// CreatedByUserID is NULL and this test fails.
func TestTmp_WriteRecordsCreatorViaShell(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-creator")
	user, err := db.CreateUser("alice-shell@example.test")
	if err != nil {
		t.Fatal(err)
	}
	fs := New(db, cust.ID, nil, WithTier("swatch"), WithUser(user.ID, mounts.OrgPerms{}))

	runShell(t, fs, `echo hi > /tmp/note`)

	row, err := db.GetTmpFile(cust.ID, "/tmp/note")
	if err != nil {
		t.Fatal(err)
	}
	if row.CreatedByUserID == nil || *row.CreatedByUserID != user.ID {
		t.Errorf("CreatedByUserID via shell write: got %v, want %q", row.CreatedByUserID, user.ID)
	}
}

// stubNoticeSink records every Maybe(key) call, in order.
// Implements shellfs.NoticeSink for tests that need to assert on
// what shellfs signals without dragging in the coordinator
// package's full NoticeQueue.
type stubNoticeSink struct {
	signaled []string
}

func (s *stubNoticeSink) Maybe(_ context.Context, key string) {
	s.signaled = append(s.signaled, key)
}

// TestTmp_FirstFileSignalsTmpExpiryNotice: the write that gives a user
// their first /tmp file signals the expiry notice; later writes don't.
//
// The gate lives here, in the emitter, on purpose. Migration 071 turned
// user_notices into a per-instance queue with no suppression, so the
// old arrangement — signal on every mutation and let the store swallow
// the repeats — would now warn the user on every command that touches
// /tmp.
func TestTmp_FirstFileSignalsTmpExpiryNotice(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-warn")
	user, _ := db.CreateUser("alice-warn@example.test")

	sink := &stubNoticeSink{}
	fs := New(db, cust.ID, nil, WithTier("swatch"),
		WithUser(user.ID, mounts.OrgPerms{}), WithNoticeQueue(sink))
	runShell(t, fs, `echo hello > /tmp/x`)

	if len(sink.signaled) != 1 || sink.signaled[0] != NoticeKeyTmpExpiry {
		t.Fatalf("first file: signaled %v, want [%q]", sink.signaled, NoticeKeyTmpExpiry)
	}

	// Appending to it, and creating a second file, are both writes the
	// user has already been warned about.
	runShell(t, fs, `echo again >> /tmp/x`)
	runShell(t, fs, `echo other > /tmp/y`)
	if len(sink.signaled) != 1 {
		t.Errorf("subsequent writes: signaled %v, want just the first", sink.signaled)
	}
}

// TestTmp_NoSignalWhenWriteFails: a failed write doesn't signal —
// shellfs only signals after a successful mutation.
func TestTmp_NoSignalWhenWriteFails(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-warn-fail")
	user, _ := db.CreateUser("alice-warn-fail@example.test")

	sink := &stubNoticeSink{}
	fs := New(db, cust.ID, nil, WithTier("trial"),
		WithUser(user.ID, mounts.OrgPerms{}), WithNoticeQueue(sink))

	// Trial cap is 500 KB. Writing 600 KB fails.
	huge := strings.Repeat("z", 600*1024)
	runShell(t, fs, `echo `+huge+` > /tmp/big`)
	if len(sink.signaled) != 0 {
		t.Errorf("failed write: signaled %v, want []", sink.signaled)
	}
}

// TestTmp_MkdirDoesNotSignal: a directory holds nothing that can
// expire, so mkdir isn't what the 48h warning is about — and a user
// whose first act is mkdir shouldn't burn the notice on it.
func TestTmp_MkdirDoesNotSignal(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-warn-mk")
	user, _ := db.CreateUser("alice-warn-mk@example.test")

	sink := &stubNoticeSink{}
	fs := New(db, cust.ID, nil, WithTier("swatch"),
		WithUser(user.ID, mounts.OrgPerms{}), WithNoticeQueue(sink))
	runShell(t, fs, `mkdir /tmp/scratch`)
	if len(sink.signaled) != 0 {
		t.Errorf("mkdir: signaled %v, want []", sink.signaled)
	}

	// The first real file still warns, mkdir notwithstanding.
	runShell(t, fs, `echo hi > /tmp/scratch/note`)
	if len(sink.signaled) != 1 || sink.signaled[0] != NoticeKeyTmpExpiry {
		t.Errorf("first file after mkdir: signaled %v, want [%q]", sink.signaled, NoticeKeyTmpExpiry)
	}
}

// TestTmp_LsLongShowsOwner: ls -l /tmp surfaces the creator user
// for each entry. Display name follows the fallback chain
// (User.Name → email local-part → 8-char UUID prefix). Files
// without a creator render as `-`.
func TestTmp_LsLongShowsOwner(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-lsowner")
	alice, _ := db.CreateUser("alice-lsl@example.test")
	// Give alice a friendly name so the rendering uses User.Name
	// rather than the email local-part.
	if err := db.SetUserName(alice.ID, "Alice"); err != nil {
		t.Fatal(err)
	}
	bob, _ := db.CreateUser("bob-lsl@example.test")
	// Bob has no Name set — should render as the email local-part.

	fsAlice := New(db, cust.ID, nil, WithTier("swatch"), WithUser(alice.ID, mounts.OrgPerms{}))
	fsBob := New(db, cust.ID, nil, WithTier("swatch"), WithUser(bob.ID, mounts.OrgPerms{}))
	// System write — no userID — should render as "-".
	fsSystem := New(db, cust.ID, nil, WithTier("swatch"))

	runShell(t, fsAlice, `echo a > /tmp/byalice`)
	runShell(t, fsBob, `echo b > /tmp/bybob`)
	runShell(t, fsSystem, `echo s > /tmp/bysystem`)

	// ls -l with a no-userID FS so the lister is a system caller.
	out, _ := runShell(t, fsSystem, `ls -l /tmp`)

	// Expect "Alice" in the row for /tmp/byalice.
	if !regexp.MustCompile(`Alice\s.*byalice`).MatchString(out) {
		t.Errorf("ls -l should render Alice as owner of byalice; got %q", out)
	}
	// Expect "bob-lsl" (email local-part) in bob's row.
	if !regexp.MustCompile(`bob-lsl\s.*bybob`).MatchString(out) {
		t.Errorf("ls -l should render bob's email local-part as owner of bybob; got %q", out)
	}
	// Expect "-" in the system row.
	if !regexp.MustCompile(`\s-\s+\d+\s.*bysystem`).MatchString(out) {
		t.Errorf("ls -l should render '-' as owner of bysystem; got %q", out)
	}
}

func TestTmp_TrialQuotaEnforcedViaShell(t *testing.T) {
	db := testStore(t)
	cust, _ := db.FindOrCreateOrg("tmp-shell-trial")
	fs := New(db, cust.ID, nil, WithTier("trial"))

	// Trial cap is 500 KB. Write two ~200 KB files, then attempt a
	// third — the last should fail with a quota error on stderr.
	big := strings.Repeat("x", 200*1024)
	runShell(t, fs, `echo `+big+` > /tmp/a`)
	runShell(t, fs, `echo `+big+` > /tmp/b`)
	_, errOut := runShell(t, fs, `echo `+big+` > /tmp/c`)
	if !strings.Contains(errOut, "quota") {
		t.Errorf("expected quota error on third write, got stderr %q", errOut)
	}
}
