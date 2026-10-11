package witrepo

import (
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/warp/pkg/gitcore"

	"github.com/graybeardcorp/weft/store"
)

type fixture struct {
	db    *store.Store
	org   store.OrgID
	alice *store.User
	bob   *store.User
}

// newFixture builds an org on a swatch plan (crons allowed) with two users,
// alice holding binmgr+cronmgr and bob holding nothing.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	t.Cleanup(func() { rollback(); db.Close() })
	org, err := tx.FindOrCreateOrg("witrepo-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.CreateSubscription(&store.Subscription{
		ID: "sub-witrepo", OrgID: org.ID, Tier: "swatch", Status: "active",
		StartsAt: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	mk := func(email, name string) *store.User {
		u, err := tx.CreateUser(email)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.SetUserName(u.ID, name); err != nil {
			t.Fatal(err)
		}
		return u
	}
	f := &fixture{db: tx, org: org.ID, alice: mk("alice-wr@example.test", "Alice"), bob: mk("bob-wr@example.test", "Bob")}
	if err := tx.DB().WithContext(store.WithOrg(t.Context(), org.ID)).Create(&store.OrgPermission{
		OrgID: org.ID, UserID: f.alice.ID, CanManageFunctions: true, CanManageWatches: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) repo(user *store.User, cwd string) *Repo {
	return New(Config{Store: f.db, OrgID: f.org, UserID: user.ID, Cwd: func() string { return cwd }})
}

func (f *fixture) saveBin(t *testing.T, u *store.User, name, src string) {
	t.Helper()
	if err := f.db.SaveShellFunction(f.org, u.ID, name, src, "bash"); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) saveCron(t *testing.T, u *store.User, name, pipeline string) {
	t.Helper()
	if err := f.db.SaveCronWatch(f.org, u.ID, name, "*/10 * * * *", pipeline, nil); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, r *Repo, sub string, args ...string) string {
	t.Helper()
	out, err := gitcore.Run(r, gitcore.Env{Product: "weft"}, sub, args)
	if err != nil {
		t.Fatalf("git %s %v: %v", sub, args, err)
	}
	return out
}

func gitErr(t *testing.T, r *Repo, sub string, args ...string) string {
	t.Helper()
	_, err := gitcore.Run(r, gitcore.Env{Product: "weft"}, sub, args)
	if err == nil {
		t.Fatalf("git %s %v: want an error", sub, args)
	}
	return err.Error()
}

func subjects(out string) []string {
	var s []string
	for _, l := range strings.Split(out, "\n") {
		if l != "" {
			s = append(s, l[strings.Index(l, " ")+1:])
		}
	}
	return s
}

func TestHistory_InterleavedBinAndCronEdits(t *testing.T) {
	f := newFixture(t)
	f.saveBin(t, f.alice, "scan", "echo 1")
	f.saveCron(t, f.alice, "tick", "echo a")
	f.saveBin(t, f.alice, "scan", "echo 2")
	f.saveCron(t, f.alice, "tick", "echo b")
	r := f.repo(f.alice, "/")

	all := subjects(git(t, r, "log", "--oneline", "--no-decorate"))
	want := []string{"Update cron.d/tick", "Update bin/scan", "Create cron.d/tick", "Create bin/scan"}
	if strings.Join(all, "|") != strings.Join(want, "|") {
		t.Errorf("org log = %v, want %v", all, want)
	}
	if got := subjects(git(t, r, "log", "--oneline", "--no-decorate", "--", "/bin/scan")); strings.Join(got, "|") != "Update bin/scan|Create bin/scan" {
		t.Errorf("log -- /bin/scan = %v", got)
	}
	if got := git(t, r, "show", "HEAD:cron.d/tick"); !strings.Contains(got, "echo b") || !strings.Contains(got, "# schedule: */10 * * * *") {
		t.Errorf("show HEAD:cron.d/tick = %q", got)
	}
	if got := git(t, r, "show", "HEAD~2:bin/scan"); got != "echo 1" {
		t.Errorf("show HEAD~2:bin/scan = %q", got)
	}
	if st := git(t, r, "status"); st != "On branch main\nnothing to commit, working tree clean" {
		t.Errorf("status = %q", st)
	}
}

func TestHistory_AuthorAndDateAreTheVersionsOwn(t *testing.T) {
	f := newFixture(t)
	f.saveBin(t, f.alice, "scan", "echo 1")
	time.Sleep(20 * time.Millisecond)
	f.saveBin(t, f.bob, "scan", "echo 2")
	r := f.repo(f.alice, "/")
	cs, _ := r.Commits()
	if len(cs) != 2 {
		t.Fatalf("%d commits", len(cs))
	}
	if cs[0].Author != "Alice" || cs[0].Email != "alice-wr@example.test" || cs[1].Author != "Bob" {
		t.Errorf("authors = %s, %s", cs[0].Author, cs[1].Author)
	}
	if !cs[1].Time.After(cs[0].Time) {
		t.Errorf("v1 %v is not older than v2 %v: dates are off by one version", cs[0].Time, cs[1].Time)
	}
}

func TestHistory_CommitIDsSurviveHeadMovingIntoHistory(t *testing.T) {
	f := newFixture(t)
	f.saveBin(t, f.alice, "scan", "echo 1")
	first, _ := f.repo(f.alice, "/").Commits()
	f.saveBin(t, f.alice, "scan", "echo 2") // v1 moves from the live row into history
	after, _ := f.repo(f.alice, "/").Commits()
	if len(after) != 2 || after[0].ID != first[0].ID {
		t.Errorf("v1's commit id changed when it became history: %v -> %v", first[0].ID, after[0].ID)
	}
}

func TestHistory_RetentionCapShowsAsABoundary(t *testing.T) {
	f := newFixture(t)
	for i := 1; i <= 8; i++ { // 5 history rows + HEAD = 6 retained; 2 trimmed
		f.saveBin(t, f.alice, "scan", "keep\necho "+string(rune('0'+i)))
	}
	r := f.repo(f.alice, "/")
	cs, _ := r.Commits()
	if len(cs) != 6 {
		t.Fatalf("retained %d versions, want 6", len(cs))
	}
	if len(cs[0].Boundary) != 1 || cs[0].Boundary[0] != "bin/scan" {
		t.Fatalf("oldest retained commit boundary = %v", cs[0].Boundary)
	}
	if out := git(t, r, "log", "--oneline"); !strings.Contains(out, "(grafted) Update bin/scan") {
		t.Errorf("no grafted marker:\n%s", out)
	}
	if out := git(t, r, "show", cs[0].ID[:7]); !strings.Contains(out, "new file mode 100644") {
		t.Errorf("boundary patch should show the file as added:\n%s", out)
	}
	blame := strings.Split(git(t, r, "blame", "bin/scan"), "\n")
	if !strings.HasPrefix(blame[0], "^"+cs[0].ID[:7]+" ") || !strings.HasSuffix(blame[0], ") keep") {
		t.Errorf("the line that predates the cap should blame to the boundary with ^:\n%s", blame[0])
	}
	if strings.HasPrefix(blame[1], "^") {
		t.Errorf("a line written after the boundary must not carry ^:\n%s", blame[1])
	}
	if msg := gitErr(t, r, "show", "HEAD~6"); !strings.Contains(msg, "bad revision") {
		t.Errorf("HEAD~6 = %q, want unresolvable", msg)
	}
}

func TestHistory_DeleteIsACommitAndRestoreBringsItBack(t *testing.T) {
	f := newFixture(t)
	f.saveBin(t, f.alice, "scan", "echo 1")
	f.saveBin(t, f.alice, "scan", "echo 2")
	if err := f.db.DeleteShellFunction(f.org, "scan"); err != nil {
		t.Fatal(err)
	}
	r := f.repo(f.alice, "/")
	if got := subjects(git(t, r, "log", "--oneline", "--no-decorate"))[0]; got != "Delete bin/scan" {
		t.Errorf("latest = %q", got)
	}
	if _, err := r.Tree(headOf(t, r)); err != nil {
		t.Fatal(err)
	}
	if tree, _ := r.Tree(headOf(t, r)); len(tree) != 0 {
		t.Errorf("deleted file still in HEAD's tree: %v", tree)
	}
	git(t, r, "restore", "--source=HEAD~1", "bin/scan")
	fn, err := f.db.GetShellFunction(f.org, "scan")
	if err != nil || fn.Source != "echo 2" {
		t.Fatalf("after restore: %v %+v", err, fn)
	}
}

func headOf(t *testing.T, r *Repo) string {
	t.Helper()
	_, id, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestWrites_RestoreRevertAndAmendGoThroughTheMounts(t *testing.T) {
	f := newFixture(t)
	f.saveBin(t, f.alice, "scan", "echo 1")
	f.saveBin(t, f.alice, "scan", "echo 2")
	f.saveBin(t, f.alice, "scan", "echo 3")
	r := f.repo(f.alice, "/bin") // cwd-relative path below

	git(t, r, "restore", "--source=HEAD~2", "scan")
	if fn, _ := f.db.GetShellFunction(f.org, "scan"); fn.Source != "echo 1" {
		t.Errorf("restore: source = %q", fn.Source)
	}
	git(t, r, "checkout", "HEAD~2", "--", "/bin/scan")
	if fn, _ := f.db.GetShellFunction(f.org, "scan"); fn.Source != "echo 3" && fn.Source != "echo 2" {
		t.Errorf("checkout -- path: source = %q", fn.Source)
	}

	// Revert the newest save (HEAD) — it is HEAD, so nothing conflicts.
	before, _ := f.db.GetShellFunction(f.org, "scan")
	out := git(t, r, "revert", "HEAD")
	if !strings.Contains(out, `Revert "`) {
		t.Errorf("revert out = %q", out)
	}
	after, _ := f.db.GetShellFunction(f.org, "scan")
	if after.Source == before.Source {
		t.Errorf("revert did not change the source (%q)", after.Source)
	}

	git(t, r, "commit", "--amend", "-m", "Tidy the scan script")
	if out := git(t, r, "log", "-1", "--oneline", "--no-decorate"); !strings.HasSuffix(out, "Tidy the scan script") {
		t.Errorf("amend not visible in log: %q", out)
	}
}

func TestWrites_ConflictingRevertWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.saveBin(t, f.alice, "scan", "echo 1")
	f.saveBin(t, f.alice, "scan", "echo 2")
	f.saveBin(t, f.alice, "scan", "echo 3")
	r := f.repo(f.alice, "/")
	msg := gitErr(t, r, "revert", "HEAD~1") // scan changed again after that save
	if !strings.Contains(msg, "could not revert") {
		t.Errorf("got %q", msg)
	}
	if fn, _ := f.db.GetShellFunction(f.org, "scan"); fn.Source != "echo 3" {
		t.Errorf("conflicting revert wrote %q", fn.Source)
	}
}

func TestWrites_MemberWithoutBinmgrCannotRestore(t *testing.T) {
	f := newFixture(t)
	f.saveBin(t, f.alice, "scan", "echo 1")
	f.saveBin(t, f.alice, "scan", "echo 2")
	r := f.repo(f.bob, "/")
	msg := gitErr(t, r, "restore", "--source=HEAD~1", "/bin/scan")
	if !strings.Contains(msg, "binmgr") {
		t.Errorf("got %q, want a binmgr denial", msg)
	}
	if fn, _ := f.db.GetShellFunction(f.org, "scan"); fn.Source != "echo 2" {
		t.Errorf("source changed to %q despite denial", fn.Source)
	}
}

func TestWrites_AmendRefusesSomeoneElsesCommit(t *testing.T) {
	f := newFixture(t)
	f.saveBin(t, f.bob, "scan", "echo 1") // bob wrote the latest version
	r := f.repo(f.alice, "/")
	if msg := gitErr(t, r, "commit", "--amend", "-m", "mine now"); !strings.Contains(msg, "someone else wrote") {
		t.Errorf("got %q", msg)
	}
}

func TestPaths_AbsoluteRelativeAndOutside(t *testing.T) {
	f := newFixture(t)
	f.saveBin(t, f.alice, "scan", "echo 1")
	r := f.repo(f.alice, "/bin")
	for _, p := range []string{"/bin/scan", "scan", "./scan", "../bin/scan"} {
		got, err := r.ResolvePath(p)
		if err != nil || got != "bin/scan" {
			t.Errorf("ResolvePath(%q) = %q, %v", p, got, err)
		}
	}
	if _, err := r.ResolvePath("/tmp/x"); err == nil || !strings.Contains(err.Error(), "outside the repository") {
		t.Errorf("outside path: %v", err)
	}
	if got := subjects(git(t, r, "log", "--oneline", "--no-decorate", "--", ".")); len(got) != 1 {
		t.Errorf("log -- . = %v", got)
	}
}

func TestUnsupportedCommandsAreExplicit(t *testing.T) {
	f := newFixture(t)
	f.saveBin(t, f.alice, "scan", "echo 1")
	r := f.repo(f.alice, "/")
	if msg := gitErr(t, r, "rebase"); msg != "git: 'rebase' is a git command, but weft doesn't support it" {
		t.Errorf("got %q", msg)
	}
	if msg := gitErr(t, r, "push"); !strings.Contains(msg, "weft doesn't support it") {
		t.Errorf("got %q", msg)
	}
	if msg := gitErr(t, r, "rebsae"); !strings.Contains(msg, "is not a git command") {
		t.Errorf("got %q", msg)
	}
}

func TestEmptyOrg(t *testing.T) {
	f := newFixture(t)
	r := f.repo(f.alice, "/")
	if msg := gitErr(t, r, "log"); !strings.Contains(msg, "does not have any commits yet") {
		t.Errorf("got %q", msg)
	}
}
