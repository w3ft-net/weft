package witrepo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/graybeardcorp/warp/pkg/gitcore"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/backends/history"
	"github.com/w3ft-net/weft/backends/history/gitrepo"
	"github.com/w3ft-net/weft/binscript"
	"github.com/w3ft-net/weft/cronauthor"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/notifycfg"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
	"github.com/w3ft-net/weft/sqlitedb"
)

// The repository over a git-backed history, SQLite stores and a Config that lets
// every user write: what an open build runs.

type user struct{ id, name, email string }

var (
	alice = user{"alice", "Alice", "alice@example.test"}
	bob   = user{"bob", "Bob", "bob@example.test"}
)

type principal struct{ user string }

func (principal) Partition() shape.Added { return shapetest.Added(1) }
func (a principal) Subject() string      { return a.user }

type allow struct{}

func (allow) Authorize(context.Context, identity.Principal, identity.Action, identity.Resource) error {
	return nil
}

type allScope struct{}

func (allScope) ReadScope(_ context.Context, a identity.Principal) (shape.Set, error) {
	return shape.Set{a.Partition().Coord()}, nil
}

type world struct {
	cfg  mounts.Config
	hist *gitrepo.Store
	dir  string
}

func newWorld(t *testing.T, mod func(*gitrepo.Config)) *world {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srcs, err := defaults.NewSourceBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	bins, err := defaults.NewBinBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	crons, err := defaults.NewCronBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	notify, err := defaults.NewNotifyBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := defaults.NewTmpBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	owner := shapetest.Added(1)
	if _, err := srcs.FindOrCreateNamespace(owner, "ops"); err != nil {
		t.Fatal(err)
	}
	gcfg := gitrepo.Config{Dir: filepath.Join(t.TempDir(), "history")}
	if mod != nil {
		mod(&gcfg)
	}
	st, err := gitrepo.Open(gcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	users := map[string]user{alice.id: alice, bob.id: bob}
	return &world{
		hist: st, dir: gcfg.Dir,
		cfg: mounts.Config{
			Owner: owner, Sources: srcs, Tmp: tmp, History: st,
			BinService: binscript.Service{Bins: bins, Crons: crons, Sources: srcs},
			Notify:     notifycfg.Service{Aliases: notify, Secrets: notify},
			CronAuthor: cronauthor.Service{Crons: crons, Sources: srcs, Bins: bins},
			Authz:      allow{}, Scope: allScope{},
			Identity: func(id string) history.Identity {
				return history.Identity{Name: users[id].name, Email: users[id].email}
			},
		},
	}
}

// repo is the repository as a user sees it.
func (w *world) repo(u user, cwd string) *Repo {
	c := w.cfg
	c.UserID, c.Principal = u.id, principal{u.id}
	return New(Config{Mounts: c, Source: w.hist, Journal: w.hist, Cwd: func() string { return cwd }})
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

func TestWrites_AreRecordedWithTheirMessageAndAuthor(t *testing.T) {
	w := newWorld(t, nil)
	r := w.repo(alice, "/")

	id, err := r.WriteFile("bin/scan", "echo 1", "Add the scan script")
	if err != nil {
		t.Fatal(err)
	}
	if fn, _ := w.cfg.BinService.Bins.Get(w.cfg.Owner, "scan"); fn == nil || fn.Source != "echo 1" || fn.Message != "Add the scan script" {
		t.Fatalf("the stored script = %+v", fn)
	}
	if b, err := os.ReadFile(filepath.Join(w.dir, "bin", "scan")); err != nil || string(b) != "echo 1" {
		t.Fatalf("history file = %q %v", b, err)
	}
	c, err := r.Commit(id)
	if err != nil || c.Message != "Add the scan script" || c.Author != "Alice" || c.Email != alice.email {
		t.Fatalf("commit = %+v %v", c, err)
	}
	// One commit for the save, not a save and then an amend.
	if cs, _ := r.Commits(); len(cs) != 1 {
		t.Fatalf("%d commits, want 1", len(cs))
	}

	// A cron, in the file form the shell shows.
	if _, err := r.WriteFile("cron.d/ops/errs", "# schedule: */10 * * * *\n/bin/scan\n", ""); err != nil {
		t.Fatal(err)
	}
	if out := git(t, r, "log", "--oneline", "--no-decorate"); strings.Join(subjects(out), "|") != "Create cron.d/ops/errs|Add the scan script" {
		t.Fatalf("log = %q", out)
	}
	if out := git(t, r, "show", "HEAD:cron.d/ops/errs"); !strings.Contains(out, "# schedule: */10 * * * *") {
		t.Fatalf("show = %q", out)
	}

	// Writing what is already there is no new commit.
	again, err := r.WriteFile("bin/scan", "echo 1", "")
	if err != nil || again != id {
		t.Fatalf("an unchanged write = %q %v, want %q", again, err, id)
	}
}

func TestRemoveAndReword_GoThroughTheMounts(t *testing.T) {
	w := newWorld(t, nil)
	r := w.repo(alice, "/")
	if _, err := r.WriteFile("bin/scan", "echo 1", ""); err != nil {
		t.Fatal(err)
	}

	git(t, r, "commit", "--amend", "-m", "Better message")
	if out := git(t, r, "log", "-1", "--oneline", "--no-decorate"); !strings.HasSuffix(out, "Better message") {
		t.Fatalf("amend not in the history: %q", out)
	}
	if fn, _ := w.cfg.BinService.Bins.Get(w.cfg.Owner, "scan"); fn.Message != "Better message" {
		t.Errorf("the stored script's message = %q", fn.Message)
	}

	// Bob did not write it.
	if msg := gitErr(t, w.repo(bob, "/"), "commit", "--amend", "-m", "Mine now"); !strings.Contains(msg, "someone else") {
		t.Fatalf("bob's amend: %q", msg)
	}

	if _, err := r.RemoveFile("bin/scan", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := w.cfg.BinService.Bins.Get(w.cfg.Owner, "scan"); err == nil {
		t.Fatal("the live script survived the removal")
	}
	if _, err := os.Stat(filepath.Join(w.dir, "bin", "scan")); !os.IsNotExist(err) {
		t.Fatalf("the file is still in the history's work tree: %v", err)
	}
	if out := git(t, r, "log", "-1", "--oneline", "--no-decorate"); !strings.HasSuffix(out, "Delete bin/scan") {
		t.Fatalf("log = %q", out)
	}
	// A deletion can't be reworded.
	if msg := gitErr(t, r, "commit", "--amend", "-m", "x"); !strings.Contains(msg, "deletion") {
		t.Fatalf("amend of a deletion: %q", msg)
	}
}

// Rewording a file's older version is refused: only the current one can be.
func TestReword_OnlyTheCurrentVersion(t *testing.T) {
	w := newWorld(t, nil)
	r := w.repo(alice, "/")
	first, err := r.WriteFile("bin/scan", "echo 1", "one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.WriteFile("bin/scan", "echo 2", "two"); err != nil {
		t.Fatal(err)
	}
	if err := r.Reword(first, "changed"); err == nil || !strings.Contains(err.Error(), "only a file's current version") {
		t.Fatalf("reword of an older version: %v", err)
	}
}

// Without a grant the mounts refuse the write, and nothing is recorded.
func TestWrites_NeedTheMountsAgreement(t *testing.T) {
	w := newWorld(t, nil)
	w.cfg.Authz = denyAll{}
	r := w.repo(alice, "/")
	if _, err := r.WriteFile("bin/scan", "echo 1", ""); err == nil {
		t.Fatal("a write the mounts refuse was accepted")
	}
	if cs, _ := r.Commits(); len(cs) != 0 {
		t.Fatalf("a refused write left %d commits", len(cs))
	}
}

type denyAll struct{}

func (denyAll) Authorize(context.Context, identity.Principal, identity.Action, identity.Resource) error {
	return identity.Denied("no")
}

// Only the tracked kinds are in the repository, and the message says which.
func TestPaths_AbsoluteRelativeAndOutside(t *testing.T) {
	r := newWorld(t, nil).repo(alice, "/bin")
	for in, want := range map[string]string{
		"/bin/scan": "bin/scan", "scan": "bin/scan", "/cron.d/ops/x": "cron.d/ops/x",
		"/etc/notify.yaml": "etc/notify.yaml", "/etc": "etc", "/": ".",
	} {
		if got, err := r.ResolvePath(in); err != nil || got != want {
			t.Errorf("ResolvePath(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	for _, out := range []string{"/tmp/x", "/etc/passwd", "/log/a"} {
		_, err := r.ResolvePath(out)
		if err == nil || !strings.Contains(err.Error(), "only /bin, /cron.d and /etc/notify.yaml are tracked") {
			t.Errorf("ResolvePath(%q) = %v, want the tracked kinds named", out, err)
		}
	}
	if _, err := r.WriteFile("tmp/x", "y", ""); err == nil || !strings.Contains(err.Error(), "not a tracked file") {
		t.Errorf("writing an untracked path: %v", err)
	}

	// A deployment adds its own kinds.
	extra := New(Config{Mounts: r.cfg.Mounts, Source: r.cfg.Source, Tracked: append(DefaultTracked[:3:3], Kind{"etc/dashboards", 1})})
	if got, err := extra.ResolvePath("/etc/dashboards/ops.yaml"); err != nil || got != "etc/dashboards/ops.yaml" {
		t.Errorf("an added kind: %q %v", got, err)
	}
	if _, err := extra.ResolvePath("/tmp/x"); err == nil || !strings.Contains(err.Error(), "/bin, /cron.d, /etc/notify.yaml and /etc/dashboards are tracked") {
		t.Errorf("the message should list the added kind: %v", err)
	}
}

// A commit that has reached the remote can't have its message changed.
func TestReword_APushedCommitIsRefused(t *testing.T) {
	var remote string
	w := newWorld(t, func(c *gitrepo.Config) {
		remote = filepath.Join(filepath.Dir(c.Dir), "origin.git")
		for _, args := range [][]string{{"init", "-q", "--bare", "-b", "main", remote}} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				panic(string(out))
			}
		}
		if err := os.MkdirAll(c.Dir, 0o755); err != nil {
			panic(err)
		}
		for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"remote", "add", "origin", remote}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = c.Dir
			if out, err := cmd.CombinedOutput(); err != nil {
				panic(string(out))
			}
		}
		c.Remote, c.Push = "origin", gitrepo.PushSync
	})
	r := w.repo(alice, "/")
	if _, err := r.WriteFile("bin/scan", "echo 1", "First"); err != nil {
		t.Fatal(err)
	}
	if msg := gitErr(t, r, "commit", "--amend", "-m", "Rewritten"); !strings.Contains(msg, "published") {
		t.Fatalf("amend of a pushed commit: %q", msg)
	}
}

// NamespaceScope is what a namespace's crons may read: the namespace itself.
func (allScope) NamespaceScope(_ context.Context, partition shape.Added, namespace string) (shape.Set, error) {
	c := partition.Coord()
	c.Namespace = shape.Value(namespace)
	return shape.Set{c}, nil
}
