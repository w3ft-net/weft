// Package gitrepo is history.Store over a real git repository, driven by the
// server's own `git` binary. Whatever the operator has configured for git
// (remotes, credentials, SSH config, hooks) applies as it would for them at a
// shell, so there is no authentication code here: to publish to GitHub or
// anywhere else, they add a remote to Dir and give the server's user the
// credentials.
//
// One Store is one group's repository. The owner the contract passes is not
// consulted: an embedder with several groups opens a Store per directory.
//
// Every save is a commit on the branch, a file at its shell path. The repository
// may be one the operator already has; weft only ever adds commits to the branch
// (and, until a commit is pushed, rewrites its message).
package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/graybeardcorp/warp/pkg/gitcore"
	"github.com/w3ft-net/weft/backends/history"
	"github.com/w3ft-net/weft/shape"
)

// Policy says when a commit is pushed to the remote.
type Policy int

const (
	// PushAsync pushes in the background, retrying with backoff while the remote
	// is unreachable: a save never waits on the network. It is the default.
	PushAsync Policy = iota
	// PushSync pushes before Record returns. A failed push is a *PushError
	// beside the commit's ID: the commit exists, it is just not off the server.
	PushSync
)

// Config configures a Store.
type Config struct {
	// Dir is the repository's work tree; it is created and initialised if it is
	// not a repository yet.
	Dir string
	// Git is the git binary; empty is "git" from the PATH.
	Git string
	// Branch is the branch commits go on; empty is the one Dir has checked out,
	// or "main" for a new repository.
	Branch string
	// Remote is the name of a remote already configured in Dir to push to; empty
	// is none, and then nothing leaves the server.
	Remote string
	Push   Policy
}

// PushError is a push that failed. The commit it was for is in the repository.
type PushError struct{ Err error }

func (e *PushError) Error() string { return "push: " + e.Err.Error() }
func (e *PushError) Unwrap() error { return e.Err }

// PushStatus is where the remote stands.
type PushStatus struct {
	Remote string
	Branch string
	// Ahead is how many commits the remote does not have.
	Ahead int
	// LastError is why the last push failed, empty if it succeeded or none ran.
	LastError string
	LastPush  time.Time
}

// Store is a git-backed history.Store.
type Store struct {
	cfg Config

	mu     sync.Mutex // serialises everything that changes the repository
	pushMu sync.Mutex // held by a push, and by a reword so it can't race one

	kick      chan struct{}
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	statMu    sync.Mutex
	lastErr   string
	lastPush  time.Time
	trees     map[string]map[string]string // commit → tree, immutable once built
	treesLock sync.Mutex
}

var _ history.Store = (*Store)(nil)

// Open opens, and if need be initialises, the repository, and starts the
// background push when a remote is configured. Close stops it.
func Open(cfg Config) (*Store, error) {
	if cfg.Dir == "" {
		return nil, errors.New("gitrepo: no directory")
	}
	if cfg.Git == "" {
		cfg.Git = "git"
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{cfg: cfg, trees: map[string]map[string]string{}}
	ctx := context.Background()
	if _, err := s.git(ctx, nil, nil, "rev-parse", "--git-dir"); err != nil {
		branch := cfg.Branch
		if branch == "" {
			branch = "main"
		}
		if _, err := s.git(ctx, nil, nil, "init", "-q", "-b", branch); err != nil {
			return nil, err
		}
	}
	if s.cfg.Branch == "" {
		out, err := s.git(ctx, nil, nil, "symbolic-ref", "-q", "--short", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("gitrepo: %s has a detached HEAD: name a branch", cfg.Dir)
		}
		s.cfg.Branch = strings.TrimSpace(string(out))
	}
	if cfg.Remote != "" {
		if _, err := s.git(ctx, nil, nil, "remote", "get-url", cfg.Remote); err != nil {
			return nil, fmt.Errorf("gitrepo: remote %q is not configured in %s", cfg.Remote, cfg.Dir)
		}
		s.kick = make(chan struct{}, 1)
		s.stop = make(chan struct{})
		s.done = make(chan struct{})
		go s.pushLoop()
		s.kickPush() // anything committed while the server was down
	}
	return s, nil
}

// Close stops the background push.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		if s.stop != nil {
			close(s.stop)
			<-s.done
		}
	})
	return nil
}

// git runs one git command in Dir. stdin may be nil; env is added to the
// process's, and git never prompts for a credential.
func (s *Store) git(ctx context.Context, env []string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, s.cfg.Git, args...)
	cmd.Dir = s.cfg.Dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Env = append(cmd.Env, env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// trackedPath cleans a contract path into one inside Dir.
func trackedPath(p string) (string, error) {
	c := filepath.ToSlash(filepath.Clean(p))
	if c == "." || strings.HasPrefix(c, "/") || c == ".." || strings.HasPrefix(c, "../") || c == ".git" || strings.HasPrefix(c, ".git/") {
		return "", fmt.Errorf("gitrepo: %q is not a path inside the repository", p)
	}
	return c, nil
}

func identEnv(who history.Identity, at time.Time) []string {
	name, email := who.Name, who.Email
	if name == "" {
		name = "system"
	}
	if email == "" {
		email = "system@weft.invalid"
	}
	date := at.Format(time.RFC3339)
	return []string{
		"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email, "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email, "GIT_COMMITTER_DATE=" + date,
	}
}

// Record commits the change.
func (s *Store) Record(ctx context.Context, _ shape.Added, c history.Change) (gitcore.ObjectID, error) {
	p, err := trackedPath(c.Path)
	if err != nil {
		return "", err
	}
	if c.At.IsZero() {
		c.At = time.Now()
	}
	id, err := s.commit(ctx, p, c)
	if err != nil {
		return "", err
	}
	switch {
	case s.cfg.Remote == "":
	case s.cfg.Push == PushSync:
		if err := s.pushNow(ctx); err != nil {
			return id, &PushError{err}
		}
	default:
		s.kickPush()
	}
	return id, nil
}

func (s *Store) commit(ctx context.Context, p string, c history.Change) (gitcore.ObjectID, error) {
	defer s.lock()()

	full := filepath.Join(s.cfg.Dir, filepath.FromSlash(p))
	tracked := s.tracked(ctx, p)
	msg := c.Message
	if c.Deleted {
		if !tracked {
			return "", history.ErrNotFound
		}
		if _, err := s.git(ctx, nil, nil, "rm", "-q", "--", p); err != nil {
			return "", err
		}
		if msg == "" {
			msg = "Delete " + p
		}
	} else {
		if tracked {
			if cur, err := os.ReadFile(full); err == nil && string(cur) == c.Content {
				return s.latest(ctx, p)
			}
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(full, []byte(c.Content), 0o644); err != nil {
			return "", err
		}
		if _, err := s.git(ctx, nil, nil, "add", "--", p); err != nil {
			return "", err
		}
		if msg == "" {
			if tracked {
				msg = "Update " + p
			} else {
				msg = "Create " + p
			}
		}
	}
	if _, err := s.git(ctx, identEnv(c.Author, c.At), nil, "commit", "-q", "--allow-empty-message", "-m", msg); err != nil {
		return "", err
	}
	out, err := s.git(ctx, nil, nil, "rev-parse", "HEAD")
	return strings.TrimSpace(string(out)), err
}

// tracked reports whether HEAD has the path.
func (s *Store) tracked(ctx context.Context, p string) bool {
	out, err := s.git(ctx, nil, nil, "ls-files", "--", p)
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func (s *Store) latest(ctx context.Context, p string) (string, error) {
	out, err := s.git(ctx, nil, nil, "log", "-1", "--format=%H", "--", p)
	if err != nil {
		// An empty repository has no HEAD to log.
		if strings.Contains(err.Error(), "does not have any commits") {
			return "", history.ErrNotFound
		}
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", history.ErrNotFound
	}
	return id, nil
}

// Latest is the newest commit that touched the path.
func (s *Store) Latest(_ shape.Added, p string) (gitcore.ObjectID, error) {
	p, err := trackedPath(p)
	if err != nil {
		return "", err
	}
	return s.latest(context.Background(), p)
}

// lock serialises whatever changes the repository, in this process by s.mu and
// across processes (the api and the coordinator record into one repository) by
// an exclusive flock on a file in .git. The returned func releases both.
func (s *Store) lock() (unlock func()) {
	s.mu.Lock()
	f, err := os.OpenFile(filepath.Join(s.cfg.Dir, ".git", "weft.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err == nil {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	}
	return func() {
		if f != nil {
			f.Close() // closing drops the flock
		}
		s.mu.Unlock()
	}
}
