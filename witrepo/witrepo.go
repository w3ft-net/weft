// Package witrepo presents an org's /bin scripts, /cron.d entries,
// /etc/dashboards and /etc/notify.yaml as a git repository, so the `git`
// command (warp's gitcore) can read and write their history.
//
// Nothing here is a git object database. Each script and cron has a live row
// plus a short history table (the newest five replaced versions), and this
// package builds from those rows one linear branch, `main`: one commit per
// saved version, ordered by when it was written, each touching one path
// (bin/<name>, cron.d/<name>, etc/dashboards/<name>.yaml or etc/notify.yaml). Writes are not staged: every write goes
// through the same mounts a `vi` or `>` write does, so permissions, language
// classification, tier limits and plan lowering all apply, and the write *is*
// the commit.
package witrepo

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/gitcore"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/mounts"
	"github.com/graybeardcorp/weft/store"
)

// Config is the request-scoped state a Repo needs.
type Config struct {
	Store  *store.Store
	OrgID  store.OrgID
	UserID string        // the acting user; empty = system call (read-only paths)
	Cwd    func() string // the shell's current directory; nil means "/"
}

// Repo implements gitcore.Repository, gitcore.AutoCommitter and
// gitcore.PathResolver over an org's /bin, /cron.d and /etc history.
type Repo struct {
	cfg   Config
	built *history // nil until first read; dropped by writes
}

// New returns a Repo for one request. History is loaded on first use.
func New(cfg Config) *Repo { return &Repo{cfg: cfg} }

var (
	_ gitcore.Repository    = (*Repo)(nil)
	_ gitcore.AutoCommitter = (*Repo)(nil)
	_ gitcore.PathResolver  = (*Repo)(nil)
)

// commitMeta ties a commit back to the row it came from.
type commitMeta struct {
	kind       string
	name       string
	objectID   uint64
	isHead     bool // the live row's current version
	tombstone  bool
	authoredBy string
}

type history struct {
	order []*gitcore.Commit
	byID  map[gitcore.ObjectID]*gitcore.Commit
	trees map[gitcore.ObjectID]map[string]string
	meta  map[gitcore.ObjectID]commitMeta
}

type event struct {
	v         store.ObjectVersion
	at        time.Time
	tombstone bool
}

// commitID names a version: sha1(kind ‖ object id ‖ version id). It does not
// change when a HEAD version moves into the history table, because version_id
// travels with the content.
func commitID(kind string, objectID uint64, versionID int64, tombstone bool) gitcore.ObjectID {
	suffix := ""
	if tombstone {
		suffix = "\x00deleted"
	}
	sum := sha1.Sum([]byte(fmt.Sprintf("%s\x00%d\x00%d%s", kind, objectID, versionID, suffix)))
	return hex.EncodeToString(sum[:])
}

// versionContent is the file content a version presents: what `cat` shows.
func versionContent(v store.ObjectVersion) string {
	if v.Kind == "cron.d" {
		return mounts.SerializeCron(&store.CronWatch{Schedule: v.Schedule, Pipeline: v.Pipeline, Enabled: true})
	}
	if v.Kind == "etc/dashboards" {
		return v.Source
	}
	if v.Language == "lua" {
		return "#!/usr/bin/env lua\n" + v.Source
	}
	return v.Source
}

func (r *Repo) load() (*history, error) {
	if r.built != nil {
		return r.built, nil
	}
	versions, err := r.cfg.Store.ListObjectVersions(r.cfg.OrgID)
	if err != nil {
		return nil, err
	}

	var events []event
	var userIDs []string
	seenUser := map[string]bool{}
	type objKey struct {
		kind string
		id   uint64
	}
	// nextAt[v] is when the next version saved under the same name was
	// written, so a deletion followed by a re-save can be kept between the
	// two. The next version is the same object when a save revives the row
	// (/bin, /cron.d) and a different one when it creates a new row
	// (/etc/dashboards), so it is keyed by name, not object.
	type nameKey struct{ kind, name string }
	var nextAt = map[int64]time.Time{}
	lastOf := map[nameKey]store.ObjectVersion{}
	byVersion := append([]store.ObjectVersion(nil), versions...)
	sort.Slice(byVersion, func(i, j int) bool { return byVersion[i].VersionID < byVersion[j].VersionID })
	for _, v := range byVersion {
		k := nameKey{v.Kind, v.Name}
		if prev, ok := lastOf[k]; ok {
			nextAt[prev.VersionID] = v.AuthoredAt
		}
		lastOf[k] = v
	}
	for _, v := range versions {
		events = append(events, event{v: v, at: v.AuthoredAt})
		if v.DeletedAt != nil {
			// The database stamps deleted_at and authored_at alike, but
			// rows deleted before migration 103 carry the app's clock:
			// never let a deletion sort before its own version or after
			// the version that revived the name.
			at := *v.DeletedAt
			if at.Before(v.AuthoredAt) {
				at = v.AuthoredAt
			}
			if next, ok := nextAt[v.VersionID]; ok && at.After(next) {
				at = next
			}
			events = append(events, event{v: v, at: at, tombstone: true})
		}
		for _, id := range []*string{v.AuthoredBy} {
			if id != nil && !seenUser[*id] {
				seenUser[*id] = true
				userIDs = append(userIDs, *id)
			}
		}
	}
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		switch {
		case !a.at.Equal(b.at):
			return a.at.Before(b.at)
		case a.v.Kind != b.v.Kind:
			return a.v.Kind < b.v.Kind
		case a.v.ObjectID != b.v.ObjectID:
			return a.v.ObjectID < b.v.ObjectID
		case a.v.VersionID != b.v.VersionID:
			return a.v.VersionID < b.v.VersionID
		}
		return !a.tombstone && b.tombstone
	})

	users, err := r.cfg.Store.GetUsersByIDs(userIDs)
	if err != nil {
		return nil, err
	}

	h := &history{
		byID:  map[gitcore.ObjectID]*gitcore.Commit{},
		trees: map[gitcore.ObjectID]map[string]string{},
		meta:  map[gitcore.ObjectID]commitMeta{},
	}
	tree := map[string]string{}
	prevName := map[objKey]string{}
	var parent gitcore.ObjectID
	for _, e := range events {
		v := e.v
		p := v.Kind + "/" + v.Name
		key := objKey{v.Kind, v.ObjectID}
		prev, seen := prevName[key]

		c := &gitcore.Commit{Time: e.at, Message: strings.TrimSpace(v.Message)}
		switch {
		case e.tombstone:
			delete(tree, p)
			c.Message = "Delete " + p
		default:
			if seen && prev != v.Name {
				delete(tree, v.Kind+"/"+prev) // rename: a delete plus an add
			}
			tree[p] = versionContent(v)
			prevName[key] = v.Name
			if c.Message == "" {
				if seen || v.Truncated {
					c.Message = "Update " + p
				} else {
					c.Message = "Create " + p
				}
			}
			if v.Truncated {
				c.Boundary = []string{p}
			}
		}
		c.ID = commitID(v.Kind, v.ObjectID, v.VersionID, e.tombstone)
		if parent != "" {
			c.Parents = []gitcore.ObjectID{parent}
		}
		c.Author, c.Email = "system", "system@weft.invalid"
		if v.AuthoredBy != nil {
			if u, ok := users[*v.AuthoredBy]; ok {
				c.Email = u.Email
				c.Author = u.Name
				if c.Author == "" {
					c.Author = strings.SplitN(u.Email, "@", 2)[0]
				}
			}
		}

		snap := make(map[string]string, len(tree))
		for k, val := range tree {
			snap[k] = val
		}
		h.order = append(h.order, c)
		h.byID[c.ID] = c
		h.trees[c.ID] = snap
		m := commitMeta{kind: v.Kind, name: v.Name, objectID: v.ObjectID, isHead: v.IsHead && !e.tombstone, tombstone: e.tombstone}
		if v.AuthoredBy != nil {
			m.authoredBy = *v.AuthoredBy
		}
		h.meta[c.ID] = m
		parent = c.ID
	}
	r.built = h
	return h, nil
}

// Head returns branch main and the org's latest change.
func (r *Repo) Head() (string, gitcore.ObjectID, error) {
	h, err := r.load()
	if err != nil {
		return "", "", err
	}
	if len(h.order) == 0 {
		return "main", "", nil
	}
	return "main", h.order[len(h.order)-1].ID, nil
}

func (r *Repo) Refs() (map[string]gitcore.ObjectID, error) {
	_, id, err := r.Head()
	if err != nil || id == "" {
		return map[string]gitcore.ObjectID{}, err
	}
	return map[string]gitcore.ObjectID{"refs/heads/main": id}, nil
}

func (r *Repo) Commits() ([]*gitcore.Commit, error) {
	h, err := r.load()
	if err != nil {
		return nil, err
	}
	return h.order, nil
}

func (r *Repo) Commit(id gitcore.ObjectID) (*gitcore.Commit, error) {
	h, err := r.load()
	if err != nil {
		return nil, err
	}
	c, ok := h.byID[id]
	if !ok {
		return nil, fmt.Errorf("no such commit %q", id)
	}
	return c, nil
}

func (r *Repo) Tree(id gitcore.ObjectID) (map[string]string, error) {
	h, err := r.load()
	if err != nil {
		return nil, err
	}
	t, ok := h.trees[id]
	if !ok {
		return nil, fmt.Errorf("no such commit %q", id)
	}
	return t, nil
}

// ResolvePath maps a typed path onto a tracked path: absolute paths as
// written, anything else relative to the shell's current directory. Paths
// outside /bin, /cron.d, /etc/dashboards and /etc/notify.yaml are not in the
// repository (the rest of /etc is generated or not versioned).
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
	switch {
	case abs == "/":
		return ".", nil // the whole repository
	case abs == "/bin" || abs == "/cron.d" || abs == "/etc" || abs == "/etc/dashboards" || abs == "/etc/notify.yaml" ||
		strings.HasPrefix(abs, "/bin/") || strings.HasPrefix(abs, "/cron.d/") || strings.HasPrefix(abs, "/etc/dashboards/"):
		return strings.TrimPrefix(abs, "/"), nil
	}
	return "", fmt.Errorf("fatal: '%s' is outside the repository (only /bin, /cron.d, /etc/dashboards and /etc/notify.yaml are tracked)", arg)
}

// splitTracked splits "bin/foo" into ("bin", "foo"), and likewise
// "etc/dashboards/x.yaml" and "etc/notify.yaml".
func splitTracked(p string) (kind, name string, err error) {
	if p == "etc/notify.yaml" {
		return "etc", "notify.yaml", nil
	}
	for _, k := range []string{"bin", "cron.d", "etc/dashboards"} {
		if rest, ok := strings.CutPrefix(p, k+"/"); ok && rest != "" && !strings.Contains(rest, "/") {
			return k, rest, nil
		}
	}
	return "", "", fmt.Errorf("fatal: '%s' is not a tracked file", p)
}

// amendMessage sets the commit message on the current version of a tracked
// file, only if authorID wrote it (see store.AmendShellFunctionMessage).
func (r *Repo) amendMessage(kind, name, authorID, message string) error {
	st, org := r.cfg.Store, r.cfg.OrgID
	switch kind {
	case "bin":
		return st.AmendShellFunctionMessage(org, authorID, name, message)
	case "cron.d":
		return st.AmendCronWatchMessage(org, authorID, name, message)
	case "etc/dashboards":
		return st.AmendDashboardMessage(org, authorID, strings.TrimSuffix(name, ".yaml"), message)
	default:
		return st.AmendNotifyAliasesMessage(org, authorID, message)
	}
}

// mountFS builds the mounts the acting user's writes go through, with the
// permission bits from their org_permissions row (no row = read-only).
func (r *Repo) mountFS() *shellcore.MountFS {
	cfg := mounts.Config{
		Store:  r.cfg.Store,
		OrgID:  r.cfg.OrgID,
		Tier:   r.cfg.Store.TierForOrg(r.cfg.OrgID),
		UserID: r.cfg.UserID,
	}
	if perm, err := r.cfg.Store.GetOrgPermission(r.cfg.OrgID, r.cfg.UserID); err == nil {
		cfg.CanManageFunctions = perm.CanManageFunctions
		cfg.CanManageWatches = perm.CanManageWatches
		cfg.CanManageEtc = perm.CanManageEtc
	}
	return mounts.New(cfg)
}

// latestFor returns the ID of the newest non-tombstone (or tombstone) commit
// on a tracked path, after a write has changed the history.
func (r *Repo) latestFor(p string, tombstone bool) (gitcore.ObjectID, error) {
	r.built = nil
	h, err := r.load()
	if err != nil {
		return "", err
	}
	for i := len(h.order) - 1; i >= 0; i-- {
		id := h.order[i].ID
		m := h.meta[id]
		if m.kind+"/"+m.name == p && m.tombstone == tombstone {
			return id, nil
		}
	}
	return "", fmt.Errorf("write to %s left no commit", p)
}

// WriteFile saves content at a tracked path through the mounts and returns
// the resulting commit. An identical write is not a new version and returns
// the path's current one.
func (r *Repo) WriteFile(p, content, message string) (gitcore.ObjectID, error) {
	kind, name, err := splitTracked(p)
	if err != nil {
		return "", err
	}
	// A save can come back with non-blocking warnings (the save-time lints);
	// the version was written, so only a rejection fails the write.
	if blocking, _ := store.Split(r.mountFS().WriteFile("/"+p, []byte(content))); len(blocking) > 0 {
		return "", errors.Join(blocking...)
	}
	id, err := r.latestFor(p, false)
	if err != nil {
		return "", err
	}
	if message != "" {
		if err := r.amendMessage(kind, name, r.cfg.UserID, message); err != nil {
			return "", err
		}
		r.built = nil
	}
	return id, nil
}

// RemoveFile deletes a tracked path through the mounts and returns the
// resulting deletion commit. The message is not stored: a deletion reads
// "Delete <path>".
func (r *Repo) RemoveFile(p, _ string) (gitcore.ObjectID, error) {
	if _, _, err := splitTracked(p); err != nil {
		return "", err
	}
	if err := r.mountFS().Remove("/" + p); err != nil {
		return "", err
	}
	return r.latestFor(p, true)
}

// Reword changes the message of a file's current version. It refuses an
// older version, a deletion, and a version someone else wrote — so a
// teammate's save that landed after yours can't be reworded by you.
func (r *Repo) Reword(id gitcore.ObjectID, message string) error {
	h, err := r.load()
	if err != nil {
		return err
	}
	m, ok := h.meta[id]
	if !ok {
		return fmt.Errorf("no such commit %q", id)
	}
	switch {
	case m.tombstone:
		return fmt.Errorf("cannot reword a deletion")
	case !m.isHead:
		return fmt.Errorf("cannot reword %s/%s: only a file's current version can be reworded", m.kind, m.name)
	case m.authoredBy != r.cfg.UserID:
		return fmt.Errorf("cannot reword %s/%s: someone else wrote that version", m.kind, m.name)
	}
	perm, err := r.cfg.Store.GetOrgPermission(r.cfg.OrgID, r.cfg.UserID)
	switch {
	case m.kind == "bin" && (err != nil || !perm.CanManageFunctions):
		return fmt.Errorf("/bin: permission denied (need the binmgr group — ask an owner for `adduser <you> binmgr`)")
	case m.kind == "cron.d" && (err != nil || !perm.CanManageWatches):
		return fmt.Errorf("/cron.d: permission denied (need the cronmgr group — ask an owner for `adduser <you> cronmgr`)")
	case strings.HasPrefix(m.kind, "etc") && (err != nil || !perm.CanManageEtc):
		return fmt.Errorf("/etc: permission denied (need the configmgr group — ask an owner for `adduser <you> configmgr`)")
	}
	err = r.amendMessage(m.kind, m.name, r.cfg.UserID, message)
	r.built = nil
	return err
}
