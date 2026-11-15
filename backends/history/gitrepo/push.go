package gitrepo

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/gitcore"
	"github.com/w3ft-net/weft/backends/history"
	"github.com/w3ft-net/weft/shape"
)

const (
	pushTimeout  = 2 * time.Minute
	retryCeiling = 5 * time.Minute
)

// retryBase is the first wait after a failed push; it doubles up to
// retryCeiling. A variable so tests need not wait.
var retryBase = 5 * time.Second

func (s *Store) kickPush() {
	select {
	case s.kick <- struct{}{}:
	default: // one is already pending, and it will push everything
	}
}

// pushLoop pushes whenever kicked, and retries a failure with backoff until it
// succeeds or the Store is closed.
func (s *Store) pushLoop() {
	defer close(s.done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	wait := retryBase
	for {
		select {
		case <-s.stop:
			return
		case <-s.kick:
		}
		for s.pushNow(ctx) != nil {
			select {
			case <-s.stop:
				return
			case <-time.After(wait):
			}
			if wait *= 2; wait > retryCeiling {
				wait = retryCeiling
			}
		}
		wait = retryBase
	}
}

// pushNow pushes the branch to the remote and records how it went.
func (s *Store) pushNow(ctx context.Context) error {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	_, err := s.git(ctx, nil, nil, "push", "-q", s.cfg.Remote, s.cfg.Branch)
	s.statMu.Lock()
	defer s.statMu.Unlock()
	if err != nil {
		s.lastErr = err.Error()
		return err
	}
	s.lastErr, s.lastPush = "", time.Now()
	return nil
}

// PushStatus is where the remote stands; the zero value if none is configured.
func (s *Store) PushStatus() PushStatus {
	if s.cfg.Remote == "" {
		return PushStatus{}
	}
	st := PushStatus{Remote: s.cfg.Remote, Branch: s.cfg.Branch}
	s.statMu.Lock()
	st.LastError, st.LastPush = s.lastErr, s.lastPush
	s.statMu.Unlock()
	rng := "HEAD"
	if _, err := s.git(context.Background(), nil, nil, "rev-parse", "-q", "--verify", s.trackingRef()); err == nil {
		rng = s.trackingRef() + "..HEAD"
	}
	if out, err := s.git(context.Background(), nil, nil, "rev-list", "--count", rng); err == nil {
		st.Ahead, _ = strconv.Atoi(strings.TrimSpace(string(out)))
	}
	return st
}

func (s *Store) trackingRef() string { return "refs/remotes/" + s.cfg.Remote + "/" + s.cfg.Branch }

// Reword sets the message of a commit that has not been pushed. The newest
// commit is amended; an older one is rewritten by a rebase that replays the
// commits after it, which therefore get new IDs. A push in flight finishes
// first, so that it cannot publish commits this is about to replace.
func (s *Store) Reword(ctx context.Context, _ shape.Added, id gitcore.ObjectID, message string) error {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	defer s.lock()()

	out, err := s.git(ctx, nil, nil, "rev-parse", "-q", "--verify", id+"^{commit}")
	if err != nil {
		return history.ErrNotFound
	}
	full := strings.TrimSpace(string(out))
	if _, err := s.git(ctx, nil, nil, "merge-base", "--is-ancestor", full, "HEAD"); err != nil {
		return history.ErrNotFound
	}
	if s.cfg.Remote != "" {
		if _, err := s.git(ctx, nil, nil, "rev-parse", "-q", "--verify", s.trackingRef()); err == nil {
			if _, err := s.git(ctx, nil, nil, "merge-base", "--is-ancestor", full, s.trackingRef()); err == nil {
				return history.ErrPublished
			}
		}
	}

	// The committer stays who it was: a reword is not a new authorship, and the
	// server needs no git identity of its own.
	who, err := s.git(ctx, nil, nil, "log", "-1", "--format=%cn%x00%ce", full)
	if err != nil {
		return err
	}
	name, email, _ := strings.Cut(strings.TrimSpace(string(who)), "\x00")
	env := []string{
		"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email,
		"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_SEQUENCE_EDITOR=true", "GIT_EDITOR=true",
	}
	head, err := s.git(ctx, nil, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(head)) == full {
		_, err = s.git(ctx, env, nil, "commit", "-q", "--amend", "--allow-empty", "--allow-empty-message", "-m", message)
		return err
	}
	if _, err := s.git(ctx, env, nil, "commit", "-q", "--allow-empty", "-m", "amend! "+full, "-m", message); err != nil {
		return err
	}
	base := []string{"rebase", "-q", "-i", "--autosquash"}
	if _, err := s.git(ctx, nil, nil, "rev-parse", "-q", "--verify", full+"^"); err == nil {
		base = append(base, full+"^")
	} else {
		base = append(base, "--root")
	}
	if _, err := s.git(ctx, env, nil, base...); err != nil {
		s.git(ctx, nil, nil, "rebase", "--abort")
		s.git(ctx, nil, nil, "reset", "-q", "--hard", strings.TrimSpace(string(head)))
		return err
	}
	return nil
}
