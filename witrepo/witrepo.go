// Package witrepo presents a tenant's tracked files (by default /bin scripts,
// /cron.d entries and /etc/notify.yaml) as a git repository, so the `git`
// command (warp's gitcore) can read and write their history.
//
// Nothing here stores history. Reads come from a history.Source, whatever a
// build keeps its history in (a real git repository, or the version rows a
// database keeps). Writes are not staged: every write goes through the same
// mounts a `vi` or `>` write does, so permissions, language classification,
// plan limits and lowering all apply, and the write *is* the commit.
package witrepo

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/graybeardcorp/warp/pkg/gitcore"
	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/w3ft-net/weft/backends/history"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/warning"
)

// Kind is a place in the filesystem whose files are tracked: a directory whose
// files are Depth path segments or fewer below it (a cron lives at
// cron.d/<namespace>/<name>, two), or, when Depth is zero, a single file.
type Kind struct {
	Path  string // without the leading slash: "bin", "etc/notify.yaml"
	Depth int
}

// DefaultTracked is what every build tracks. A deployment adds the kinds it has
// (hosted: etc/dashboards).
var DefaultTracked = []Kind{{"bin", 1}, {"cron.d", 2}, {"etc/notify.yaml", 0}}

// Config is the request-scoped state a Repo needs.
type Config struct {
	// Mounts is the request's mounts Config: who acts, and the services their
	// writes go through. If it has a History, writes are recorded there.
	Mounts mounts.Config
	// Source is the history git reads.
	Source history.Source
	// Journal is where a reword is recorded when the history has a record that
	// holds messages (a git repository); nil when the message lives only in the
	// stored versions, which an amend through the mounts changes.
	Journal history.Journal
	// Cwd is the shell's current directory; nil means "/".
	Cwd func() string
	// Tracked says which files are in the repository; nil is DefaultTracked.
	Tracked []Kind
}

// Repo implements gitcore.Repository, gitcore.AutoCommitter and
// gitcore.PathResolver over a tenant's tracked files.
type Repo struct {
	cfg Config
	src gitcore.Repository // the Source's repository, dropped by writes
}

// New returns a Repo for one request. The history is read on first use.
func New(cfg Config) *Repo {
	if cfg.Tracked == nil {
		cfg.Tracked = DefaultTracked
	}
	return &Repo{cfg: cfg}
}

var (
	_ gitcore.Repository    = (*Repo)(nil)
	_ gitcore.AutoCommitter = (*Repo)(nil)
	_ gitcore.PathResolver  = (*Repo)(nil)
)

func (r *Repo) owner() shape.Added { return r.cfg.Mounts.Partition() }

// repo is the Source's repository, read once until a write changes it.
func (r *Repo) repo() (gitcore.Repository, error) {
	if r.src == nil {
		src, err := r.cfg.Source.Repository(r.owner())
		if err != nil {
			return nil, err
		}
		r.src = src
	}
	return r.src, nil
}

func (r *Repo) Head() (string, gitcore.ObjectID, error) {
	src, err := r.repo()
	if err != nil {
		return "", "", err
	}
	return src.Head()
}

func (r *Repo) Refs() (map[string]gitcore.ObjectID, error) {
	src, err := r.repo()
	if err != nil {
		return nil, err
	}
	return src.Refs()
}

func (r *Repo) Commits() ([]*gitcore.Commit, error) {
	src, err := r.repo()
	if err != nil {
		return nil, err
	}
	return src.Commits()
}

func (r *Repo) Commit(id gitcore.ObjectID) (*gitcore.Commit, error) {
	src, err := r.repo()
	if err != nil {
		return nil, err
	}
	return src.Commit(id)
}

func (r *Repo) Tree(id gitcore.ObjectID) (map[string]string, error) {
	src, err := r.repo()
	if err != nil {
		return nil, err
	}
	return src.Tree(id)
}

// ResolvePath maps a typed path onto a tracked path: absolute paths as written,
// anything else relative to the shell's current directory. Paths outside the
// tracked kinds are not in the repository (the rest of /etc is generated or not
// versioned).
func (r *Repo) ResolvePath(arg string) (string, error) {
	abs := arg
	if !strings.HasPrefix(arg, "/") {
		cwd := "/"
		if r.cfg.Cwd != nil {
			cwd = r.cfg.Cwd()
		}
		abs = path.Join(cwd, arg)
	}
	abs = path.Clean(abs)
	if abs == "/" {
		return ".", nil // the whole repository
	}
	p := strings.TrimPrefix(abs, "/")
	for _, k := range r.cfg.Tracked {
		// The kind itself, what is under it, or a directory that holds it (/etc).
		if p == k.Path || strings.HasPrefix(p, k.Path+"/") || strings.HasPrefix(k.Path, p+"/") {
			return p, nil
		}
	}
	return "", fmt.Errorf("fatal: '%s' is outside the repository (only %s are tracked)", arg, r.trackedList())
}

// trackedList reads the tracked kinds as a sentence: "/bin, /cron.d and /etc/notify.yaml".
func (r *Repo) trackedList() string {
	var names []string
	for _, k := range r.cfg.Tracked {
		names = append(names, "/"+k.Path)
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// tracked reports whether p is a file of a tracked kind.
func (r *Repo) tracked(p string) error {
	for _, k := range r.cfg.Tracked {
		if k.Depth == 0 {
			if p == k.Path {
				return nil
			}
			continue
		}
		rest, ok := strings.CutPrefix(p, k.Path+"/")
		if ok && rest != "" && strings.Count(rest, "/") <= k.Depth-1 && !strings.HasSuffix(rest, "/") && !strings.HasPrefix(rest, "/") {
			return nil
		}
	}
	return fmt.Errorf("fatal: '%s' is not a tracked file", p)
}

// mountFS builds the mounts the acting user's writes go through, under the
// Config's own answer for that user (no grant means read-only). message, when
// there is one, goes to the history in the commit itself.
func (r *Repo) mountFS(message string) *shellcore.MountFS {
	cfg := r.cfg.Mounts
	if message != "" {
		base := cfg.Ctx
		if base == nil {
			base = context.Background()
		}
		cfg.Ctx = history.WithMessage(base, message)
	}
	return mounts.New(cfg)
}

// amendRow sets the message on the stored version of a tracked file, only if the
// acting user wrote it. The history is left alone: a commit's message is set
// when it is made, or by Journal.Reword.
func (r *Repo) amendRow(p, message string) error {
	cfg := r.cfg.Mounts
	cfg.History = nil
	return mounts.AmendVersionMessage(cfg, "/"+p, message)
}

// WriteFile saves content at a tracked path through the mounts and returns the
// resulting commit. An identical write is not a new version and returns the
// path's current one.
func (r *Repo) WriteFile(p, content, message string) (gitcore.ObjectID, error) {
	if err := r.tracked(p); err != nil {
		return "", err
	}
	// A save can come back with non-blocking warnings (the save-time lints); the
	// version was written, so only a rejection fails the write.
	if blocking, _ := warning.Split(r.mountFS(message).WriteFile("/"+p, []byte(content))); len(blocking) > 0 {
		return "", errors.Join(blocking...)
	}
	r.src = nil
	id, err := r.cfg.Source.Latest(r.owner(), p)
	if err != nil {
		return "", fmt.Errorf("write to %s left no commit: %w", p, err)
	}
	if message != "" {
		if err := r.amendRow(p, message); err != nil {
			return "", err
		}
		r.src = nil
	}
	return id, nil
}

// RemoveFile deletes a tracked path through the mounts and returns the resulting
// deletion commit. The message is not stored: a deletion reads "Delete <path>".
func (r *Repo) RemoveFile(p, _ string) (gitcore.ObjectID, error) {
	if err := r.tracked(p); err != nil {
		return "", err
	}
	if err := r.mountFS("").Remove("/" + p); err != nil {
		return "", err
	}
	r.src = nil
	return r.cfg.Source.Latest(r.owner(), p)
}

// commitPath is the one file a commit changed, and whether it is there after
// the commit (false for a deletion). A rename is a deletion and an addition, and
// is the addition.
func (r *Repo) commitPath(id gitcore.ObjectID) (p string, present bool, err error) {
	c, err := r.Commit(id)
	if err != nil {
		return "", false, err
	}
	after, err := r.Tree(id)
	if err != nil {
		return "", false, err
	}
	before := map[string]string{}
	if len(c.Parents) > 0 {
		if before, err = r.Tree(c.Parents[0]); err != nil {
			return "", false, err
		}
	}
	var written, removed []string
	for k, v := range after {
		if old, ok := before[k]; !ok || old != v {
			written = append(written, k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			removed = append(removed, k)
		}
	}
	switch {
	case len(written) == 1:
		return written[0], true, nil
	case len(written) == 0 && len(removed) == 1:
		return removed[0], false, nil
	}
	return "", false, fmt.Errorf("cannot reword %s: it changed %d files, not one", id, len(written)+len(removed))
}

// identity is how the history names the acting user.
func (r *Repo) identity() history.Identity {
	c := r.cfg.Mounts
	if c.UserID == "" {
		return history.Identity{}
	}
	if c.Identity != nil {
		return c.Identity(c.UserID)
	}
	return history.Identity{Name: c.UserID}
}

// Reword changes the message of a file's current version. It refuses an older
// version, a deletion, and a version someone else wrote, so a teammate's save that
// landed after yours can't be reworded by you; and the history refuses a commit
// that has been pushed.
func (r *Repo) Reword(id gitcore.ObjectID, message string) error {
	p, present, err := r.commitPath(id)
	if err != nil {
		return err
	}
	if err := r.tracked(p); err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("cannot reword a deletion")
	}
	if cur, err := r.cfg.Source.Latest(r.owner(), p); err != nil || cur != id {
		return fmt.Errorf("cannot reword %s: only a file's current version can be reworded", p)
	}
	c, err := r.Commit(id)
	if err != nil {
		return err
	}
	if who := r.identity(); c.Email != who.Email || c.Author != who.Name {
		return fmt.Errorf("cannot reword %s: someone else wrote that version", p)
	}
	// Rewording is a write to the file, so it takes the same group as any other
	// write: ask the mounts, rather than repeating their check here.
	if err := r.mountFS("").Access("/"+p, shellcore.PermWrite); err != nil {
		return err
	}
	if r.cfg.Journal != nil {
		if err := r.cfg.Journal.Reword(context.Background(), r.owner(), id, message); err != nil {
			return err
		}
	}
	err = r.amendRow(p, message)
	r.src = nil
	return err
}
