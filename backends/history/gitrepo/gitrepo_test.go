package gitrepo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/history"
	"github.com/w3ft-net/weft/shape"
)

var (
	ctx   = context.Background()
	owner shape.Added
	ada   = history.Identity{Name: "Ada", Email: "ada@example.com"}
	bob   = history.Identity{Name: "Bob", Email: "bob@example.com"}
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func open(t *testing.T, mod func(*Config)) *Store {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
	retryBase = 20 * time.Millisecond
	cfg := Config{Dir: filepath.Join(t.TempDir(), "repo")}
	if mod != nil {
		mod(&cfg)
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func save(t *testing.T, s *Store, who history.Identity, p, content, msg string) string {
	t.Helper()
	id, err := s.Record(ctx, owner, history.Change{Path: p, Content: content, Author: who, Message: msg})
	if err != nil {
		t.Fatalf("Record %s: %v", p, err)
	}
	return id
}

func messages(t *testing.T, s *Store) []string {
	t.Helper()
	r, _ := s.Repository(owner)
	_, head, _ := r.Head()
	var out []string
	for id := head; id != ""; {
		c, err := r.Commit(id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c.Message)
		id = ""
		if len(c.Parents) > 0 {
			id = c.Parents[0]
		}
	}
	return out
}

func TestRecord_BuildsTheHistoryAtShellPaths(t *testing.T) {
	s := open(t, nil)
	r, _ := s.Repository(owner)
	if ref, id, err := r.Head(); err != nil || ref != "main" || id != "" {
		t.Fatalf("empty repo: Head = %q %q %v", ref, id, err)
	}
	if cs, err := r.Commits(); err != nil || len(cs) != 0 {
		t.Fatalf("empty repo: Commits = %v %v", cs, err)
	}

	a := save(t, s, ada, "bin/scan", "echo 1\n", "")
	b := save(t, s, bob, "bin/scan", "echo 2\n", "Count twice")
	c := save(t, s, ada, "cron.d/ops/nightly", "every 5m\n/bin/scan\n", "")
	if _, err := s.Record(ctx, owner, history.Change{Path: "bin/scan", Deleted: true, Author: bob}); err != nil {
		t.Fatal(err)
	}

	want := []string{"Delete bin/scan", "Create cron.d/ops/nightly", "Count twice", "Create bin/scan"}
	if got := messages(t, s); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("messages = %q, want %q", got, want)
	}
	cm, err := r.Commit(b)
	if err != nil || cm.Author != "Bob" || cm.Email != "bob@example.com" || len(cm.Parents) != 1 || cm.Parents[0] != a {
		t.Fatalf("commit b = %+v %v", cm, err)
	}
	tree, err := r.Tree(c)
	if err != nil || tree["bin/scan"] != "echo 2\n" || tree["cron.d/ops/nightly"] != "every 5m\n/bin/scan\n" {
		t.Fatalf("tree at c = %v %v", tree, err)
	}
	_, head, _ := r.Head()
	if tree, _ := r.Tree(head); len(tree) != 1 || tree["bin/scan"] != "" {
		t.Fatalf("tree at head = %v, want only the cron", tree)
	}
	if refs, _ := r.Refs(); refs["refs/heads/main"] != head {
		t.Fatalf("refs = %v", refs)
	}
	if _, err := r.Commit("0000000"); err == nil {
		t.Fatal("a commit that doesn't exist was found")
	}
}

func TestRecord_UnchangedContentIsNotAChange(t *testing.T) {
	s := open(t, nil)
	a := save(t, s, ada, "bin/scan", "echo 1\n", "")
	again := save(t, s, bob, "bin/scan", "echo 1\n", "")
	if again != a {
		t.Fatalf("an unchanged save made commit %s, want %s", again, a)
	}
	if got := messages(t, s); len(got) != 1 {
		t.Fatalf("messages = %q", got)
	}
}

func TestLatestAndNotFound(t *testing.T) {
	s := open(t, nil)
	if _, err := s.Latest(owner, "bin/x"); !errors.Is(err, history.ErrNotFound) {
		t.Fatalf("Latest on an empty repo: %v", err)
	}
	a := save(t, s, ada, "bin/x", "1\n", "")
	save(t, s, ada, "bin/y", "1\n", "")
	if got, err := s.Latest(owner, "bin/x"); err != nil || got != a {
		t.Fatalf("Latest = %q %v, want %q", got, err, a)
	}
	if _, err := s.Record(ctx, owner, history.Change{Path: "bin/missing", Deleted: true}); !errors.Is(err, history.ErrNotFound) {
		t.Fatalf("deleting a path with no history: %v", err)
	}
	for _, bad := range []string{"", "../x", "/etc/passwd", ".git/config"} {
		if _, err := s.Record(ctx, owner, history.Change{Path: bad, Content: "x"}); err == nil {
			t.Errorf("path %q was accepted", bad)
		}
	}
}

func TestReword_NewestAndOlderUnpushed(t *testing.T) {
	s := open(t, nil)
	a := save(t, s, ada, "bin/a", "1\n", "first")
	save(t, s, ada, "bin/b", "1\n", "second")
	c := save(t, s, ada, "bin/c", "1\n", "third")

	if err := s.Reword(ctx, owner, c, "third, better"); err != nil {
		t.Fatalf("reword newest: %v", err)
	}
	if err := s.Reword(ctx, owner, a, "first, better"); err != nil {
		t.Fatalf("reword oldest: %v", err)
	}
	want := "third, better|second|first, better"
	if got := strings.Join(messages(t, s), "|"); got != want {
		t.Fatalf("messages = %q, want %q", got, want)
	}
	r, _ := s.Repository(owner)
	_, head, _ := r.Head()
	if tree, _ := r.Tree(head); len(tree) != 3 || tree["bin/a"] != "1\n" {
		t.Fatalf("a reword changed the files: %v", tree)
	}
	cs, _ := r.Commits()
	for _, c := range cs {
		if c.Author != "Ada" {
			t.Errorf("a reword changed the author of %q to %q", c.Message, c.Author)
		}
	}
	if err := s.Reword(ctx, owner, "deadbeef", "x"); !errors.Is(err, history.ErrNotFound) {
		t.Fatalf("reword of a missing commit: %v", err)
	}
}

// bare makes a bare repository to stand in for GitHub, and a Store that pushes to it.
func withRemote(t *testing.T, policy Policy) (*Store, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
	remote := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, t.TempDir(), "init", "-q", "--bare", "-b", "main", remote)
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "remote", "add", "origin", remote)
	s := open(t, func(c *Config) { c.Dir, c.Remote, c.Push = dir, "origin", policy })
	return s, remote
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPush_AsyncReachesTheRemoteWithOperatorsFiles(t *testing.T) {
	s, remote := withRemote(t, PushAsync)
	id := save(t, s, ada, "bin/scan", "echo 1\n", "")
	eventually(t, "the remote to have the commit", func() bool {
		return s.PushStatus().Ahead == 0 && s.PushStatus().LastPush != time.Time{}
	})
	if got := runGit(t, remote, "rev-parse", "main"); got != id {
		t.Fatalf("remote main = %s, want %s", got, id)
	}
	// Someone cloning it gets the weft tree.
	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, t.TempDir(), "clone", "-q", remote, clone)
	if b, err := os.ReadFile(filepath.Join(clone, "bin", "scan")); err != nil || string(b) != "echo 1\n" {
		t.Fatalf("clone bin/scan = %q %v", b, err)
	}
}

func TestPush_SyncFailureIsAPushErrorBesideTheCommit(t *testing.T) {
	s, remote := withRemote(t, PushSync)
	id, err := s.Record(ctx, owner, history.Change{Path: "bin/a", Content: "1\n", Author: ada})
	if err != nil || runGit(t, remote, "rev-parse", "main") != id {
		t.Fatalf("a push to a healthy remote: %v", err)
	}
	moved := remote + ".away"
	if err := os.Rename(remote, moved); err != nil {
		t.Fatal(err)
	}
	id2, err := s.Record(ctx, owner, history.Change{Path: "bin/b", Content: "1\n", Author: ada})
	var pe *PushError
	if !errors.As(err, &pe) || id2 == "" {
		t.Fatalf("Record with the remote gone = %q %v, want the commit and a *PushError", id2, err)
	}
	if st := s.PushStatus(); st.Ahead != 1 || st.LastError == "" {
		t.Fatalf("status = %+v", st)
	}
	if r, _ := s.Repository(owner); r != nil {
		if _, head, _ := r.Head(); head != id2 {
			t.Fatal("the commit that failed to push is not in the repository")
		}
	}
}

func TestPush_AsyncRetriesUntilTheRemoteIsBack(t *testing.T) {
	s, remote := withRemote(t, PushAsync)
	moved := remote + ".away"
	if err := os.Rename(remote, moved); err != nil {
		t.Fatal(err)
	}
	id := save(t, s, ada, "bin/a", "1\n", "")
	eventually(t, "a failed push to be reported", func() bool { return s.PushStatus().LastError != "" })
	if st := s.PushStatus(); st.Ahead != 1 {
		t.Fatalf("status while down = %+v", st)
	}
	if err := os.Rename(moved, remote); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the retry to land", func() bool { return s.PushStatus().Ahead == 0 })
	if got := runGit(t, remote, "rev-parse", "main"); got != id {
		t.Fatalf("remote main = %s, want %s", got, id)
	}
	if s.PushStatus().LastError != "" {
		t.Fatalf("the error outlived the success: %+v", s.PushStatus())
	}
}

func TestReword_RefusesWhatHasBeenPushed(t *testing.T) {
	s, remote := withRemote(t, PushSync)
	a := save(t, s, ada, "bin/a", "1\n", "first")
	// A later commit that cannot be pushed stays rewordable.
	moved := remote + ".away"
	if err := os.Rename(remote, moved); err != nil {
		t.Fatal(err)
	}
	b, _ := s.Record(ctx, owner, history.Change{Path: "bin/b", Content: "1\n", Author: ada, Message: "second"}) // the push fails; the commit stands

	if err := s.Reword(ctx, owner, a, "changed"); !errors.Is(err, history.ErrPublished) {
		t.Fatalf("reword of a pushed commit: %v", err)
	}
	if err := s.Reword(ctx, owner, b, "second, changed"); err != nil {
		t.Fatalf("reword of an unpushed commit: %v", err)
	}
	if got := messages(t, s); got[0] != "second, changed" || got[1] != "first" {
		t.Fatalf("messages = %q", got)
	}
}

func TestOpen_PushesWhatWasCommittedWhileTheServerWasDown(t *testing.T) {
	s, remote := withRemote(t, PushAsync)
	dir := s.cfg.Dir
	s.Close()
	s.cfg.Remote = ""
	id := save(t, s, ada, "bin/a", "1\n", "")
	s2, err := Open(Config{Dir: dir, Remote: "origin"})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	eventually(t, "the backlog to push", func() bool { return s2.PushStatus().Ahead == 0 })
	if got := runGit(t, remote, "rev-parse", "main"); got != id {
		t.Fatalf("remote main = %s, want %s", got, id)
	}
}

func TestOpen_RefusesARemoteThatIsNotConfigured(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
	if _, err := Open(Config{Dir: filepath.Join(t.TempDir(), "r"), Remote: "origin"}); err == nil {
		t.Fatal("Open accepted a remote the repository doesn't have")
	}
}

func TestOpen_AdoptsAnExistingRepositoryAndItsBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "trunk")
	s, err := Open(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	save(t, s, ada, "bin/a", "1\n", "")
	r, _ := s.Repository(owner)
	if ref, _, _ := r.Head(); ref != "trunk" {
		t.Fatalf("branch = %q, want the one the repository had", ref)
	}
}

// The api and the coordinator are two processes that record into one
// repository. Two Stores on a directory stand for them: their commits must
// serialise, not fail on git's index lock.
func TestRecord_TwoStoresOnOneDirectoryDoNotCollide(t *testing.T) {
	a := open(t, nil)
	b, err := Open(Config{Dir: a.cfg.Dir})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	const n = 20
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		for j, s := range []*Store{a, b} {
			go func() {
				_, err := s.Record(ctx, owner, history.Change{Path: "bin/s" + string(rune('a'+j)) + string(rune('a'+i)), Content: "x", Author: ada})
				errs <- err
			}()
		}
	}
	for i := 0; i < 2*n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("a concurrent Record failed: %v", err)
		}
	}
	if got := len(messages(t, a)); got != 2*n {
		t.Errorf("history has %d commits, want %d", got, 2*n)
	}
}
