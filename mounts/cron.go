package mounts

import (
	"fmt"
	"github.com/w3ft-net/weft/cronauthor"
	"io/fs"
	"slices"
	"sort"
	"strings"

	"github.com/w3ft-net/weft/backends/cron"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/w3ft-net/weft/identity"
)

type CronMount struct{ cfg Config }

func (m *CronMount) Prefix() string { return "/cron.d" }

func (m *CronMount) Caps() shellcore.MountCaps {
	return shellcore.MountCaps{Writable: true}
}

// cronPath is a /cron.d path resolved: the root, a namespace directory, or a
// cron. A cron is homed in a namespace (ns set) or at the org level.
type cronPath struct {
	root bool
	dir  bool   // a namespace directory
	ns   string // the home namespace's name; "" for an org-level cron
	name string // the cron's name; "" for the root and namespace directories
}

// resolve reads rel against the org's namespaces. A first segment that names a
// namespace is that namespace's directory; otherwise it is an org-level cron
// (a cron and a namespace never share a name).
func (m *CronMount) resolve(rel string) (cronPath, error) {
	if rel == "" {
		return cronPath{root: true}, nil
	}
	first, rest, more := strings.Cut(rel, "/")
	_, isNS, err := m.cfg.cronAuthor().Sources.GetNamespace(m.cfg.partition(), first)
	if err != nil {
		return cronPath{}, err
	}
	switch {
	case !more && isNS:
		return cronPath{dir: true, ns: first}, nil
	case !more:
		return cronPath{name: first}, nil
	case isNS && rest != "" && !strings.Contains(rest, "/"):
		return cronPath{ns: first, name: rest}, nil
	}
	return cronPath{}, fs.ErrNotExist
}

// Authorize is the mount's one access check, called by MountFS before every
// operation. A namespace's crons are visible to whoever can read the
// namespace, and writable by whoever may manage crons in it. Org-level crons
// are visible to every member and published by PublishOrgWide.
func (m *CronMount) Authorize(rel string, want shellcore.Perm) error {
	p, err := m.resolve(rel)
	if err != nil {
		return err
	}
	if p.ns != "" {
		readable, _, err := m.cfg.scope()
		if err != nil {
			return err
		}
		if !slices.Contains(readable, p.ns) {
			return fs.ErrNotExist
		}
	}
	if want&shellcore.PermWrite == 0 {
		return nil
	}
	switch {
	case p.root || p.dir:
		return fmt.Errorf("/cron.d/%s: %w (namespace directories come with the namespace; write a cron at /cron.d/<namespace>/<name>)", rel, fs.ErrPermission)
	case p.ns != "":
		if !m.cfg.allowedIn(identity.ActionManageCron, p.ns) {
			return denyNamespaceWrite("/cron.d/"+p.ns, p.ns)
		}
	default:
		// Org-level crons are on their way out: an existing one stays editable
		// by an owner until it is moved, but no new one can be made.
		if _, err := m.get(rel); err != nil {
			return fmt.Errorf("/cron.d/%s: %w (a cron belongs to a namespace; make it at /cron.d/<namespace>/%s)", p.name, fs.ErrPermission, p.name)
		}
		if !m.cfg.allowed(identity.ActionPublishOrgWide) {
			return fmt.Errorf("/cron.d/%s: %w (an org-level cron can only be changed by an owner; move it into a namespace with `mv /cron.d/%s /cron.d/<namespace>/%s`)", p.name, fs.ErrPermission, p.name, p.name)
		}
	}
	return nil
}

// home is the namespace id of a cron's home, 0 at the org level.
func (m *CronMount) home(p cronPath) (uint, error) {
	if p.ns == "" {
		return 0, nil
	}
	ns, found, err := m.cfg.cronAuthor().Sources.GetNamespace(m.cfg.partition(), p.ns)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fs.ErrNotExist
	}
	return ns.ID, nil
}

func (m *CronMount) ReadDir(rel string) ([]fs.DirEntry, error) {
	p, err := m.resolve(rel)
	if err != nil {
		return nil, err
	}
	if !p.root && !p.dir {
		return nil, fs.ErrNotExist
	}
	watches, err := m.cfg.cronAuthor().Crons.ListWatches(m.cfg.partition())
	if err != nil {
		return nil, err
	}
	var entries []fs.DirEntry
	if p.root {
		readable, _, err := m.cfg.scope()
		if err != nil {
			return nil, err
		}
		for _, ns := range readable {
			entries = append(entries, &vfsDirEntry{name: ns, dir: true})
		}
	}
	home, err := m.home(p)
	if err != nil {
		return nil, err
	}
	var here []cron.Watch
	for _, w := range watches {
		if w.Namespace == home {
			here = append(here, w)
		}
	}
	ids := make([]string, 0, len(here))
	for _, w := range here {
		if w.CreatedBy != "" {
			ids = append(ids, w.CreatedBy)
		}
	}
	owners := m.cfg.OwnerNames(ids)
	for _, w := range here {
		owner := owners[w.CreatedBy]
		entries = append(entries, &vfsDirEntry{
			name:  w.Name,
			size:  int64(len(serialize(w))),
			mtime: w.UpdatedAt,
			owner: owner,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

// get finds the cron at rel.
func (m *CronMount) get(rel string) (*cron.Watch, error) {
	p, err := m.resolve(rel)
	if err != nil || p.name == "" {
		return nil, fs.ErrNotExist
	}
	home, err := m.home(p)
	if err != nil {
		return nil, fs.ErrNotExist
	}
	w, err := m.cfg.cronAuthor().Crons.GetWatch(m.cfg.partition(), home, p.name)
	if err != nil {
		return nil, fs.ErrNotExist
	}
	return w, nil
}

func (m *CronMount) ReadFile(rel string) ([]byte, error) {
	w, err := m.get(rel)
	if err != nil {
		return nil, fmt.Errorf("/cron.d/%s: no such file", rel)
	}
	return []byte(serialize(*w)), nil
}

func (m *CronMount) Stat(rel string) (fs.FileInfo, error) {
	p, err := m.resolve(rel)
	if err != nil {
		return nil, err
	}
	if p.root || p.dir {
		name := "cron.d"
		if p.dir {
			name = p.ns
		}
		return &vfsFileInfo{name: name, dir: true}, nil
	}
	w, err := m.get(rel)
	if err != nil {
		return nil, fs.ErrNotExist
	}
	return &vfsFileInfo{name: p.name, size: int64(len(serialize(*w))), mtime: w.UpdatedAt}, nil
}

func (m *CronMount) writeFile(rel string, data []byte) error {
	p, err := m.resolve(rel)
	if err != nil {
		return err
	}
	if p.root || p.dir {
		return fmt.Errorf("/cron.d/%s: is a directory", rel)
	}
	home, err := m.home(p)
	if err != nil {
		return err
	}
	schedule, pipeline, enabled, err := cronauthor.Parse(string(data))
	if err != nil {
		return fmt.Errorf("/cron.d/%s: %w", rel, err)
	}

	// A namespace's cron has that namespace's authority and no more.
	authorize := m.cfg.authorizeCronReads
	if p.ns != "" {
		authorize = func(pipeline string) error { return m.cfg.authorizeHomedCronReads(p.ns, pipeline) }
	}
	return m.cfg.cronAuthor().Save(m.cfg.ctx(), cronauthor.Request{
		Owner: m.cfg.partition(), Home: home, User: m.cfg.UserID, Name: p.name,
		Schedule: schedule, Pipeline: pipeline, Enabled: enabled, Authorize: authorize,
	})
}

func (m *CronMount) remove(rel string) error {
	p, err := m.resolve(rel)
	if err != nil {
		return err
	}
	if p.name == "" {
		return fmt.Errorf("/cron.d/%s: is a directory; it goes away with its namespace", rel)
	}
	home, err := m.home(p)
	if err != nil {
		return err
	}
	return m.cfg.cronAuthor().Crons.DeleteWatch(m.cfg.partition(), home, p.name)
}

// Rename moves a cron to another home, or renames it, as the same row, so its
// history and results come with it. MountFS has already authorized a write at
// both paths.
func (m *CronMount) rename(oldRel, newRel string) error {
	from, err := m.resolve(oldRel)
	if err != nil {
		return err
	}
	to, err := m.resolve(newRel)
	if err != nil {
		return err
	}
	if from.name == "" || to.name == "" {
		return fmt.Errorf("/cron.d/%s: is a directory; a namespace's directory comes with the namespace", map[bool]string{true: oldRel, false: newRel}[from.name == ""])
	}
	cron, err := m.get(oldRel)
	if err != nil {
		return err
	}
	if _, err := m.get(newRel); err == nil {
		return fmt.Errorf("/cron.d/%s: already exists", newRel)
	}
	fromHome, err := m.home(from)
	if err != nil {
		return err
	}
	toHome, err := m.home(to)
	if err != nil {
		return err
	}
	if to.ns != "" {
		if err := m.cfg.authorizeHomedCronReads(to.ns, cron.Pipeline); err != nil {
			return err
		}
	} else if to.ns == "" && from.ns != "" {
		return fmt.Errorf("/cron.d/%s: %w (a cron belongs to a namespace)", newRel, fs.ErrPermission)
	}
	if err := m.cfg.cronAuthor().Crons.MoveWatch(m.cfg.partition(), fromHome, from.name, toHome, to.name); err != nil {
		return err
	}
	if m.cfg.CronRenamed != nil && from.ns == "" {
		return m.cfg.CronRenamed(m.cfg, from.name, newRel)
	}
	return nil
}

// AmendVersionMessage allows recording a commit message for cron entries. It
// is not a MountFS operation, so it checks write access itself.
func (m *CronMount) AmendVersionMessage(rel, message string) error {
	if err := m.Authorize(rel, shellcore.PermWrite); err != nil {
		return err
	}
	p, err := m.resolve(rel)
	if err != nil || p.name == "" {
		return fs.ErrNotExist
	}
	home, err := m.home(p)
	if err != nil {
		return err
	}
	if err := m.cfg.cronAuthor().Crons.AmendMessage(m.cfg.partition(), home, m.cfg.UserID, p.name, message); err != nil {
		return err
	}
	return m.cfg.RewordHistory("cron.d/"+rel, message)
}

// serialize is a cron as the shell shows it, the one form (cronauthor.Serialize).
func serialize(w cron.Watch) string {
	return cronauthor.Serialize(w.Schedule, w.Pipeline, w.Enabled)
}

// cronAuthor is the cron writing service the Config was given.
func (c Config) cronAuthor() cronauthor.Service { return c.CronAuthor }

func (m *CronMount) WriteFile(rel string, data []byte) error {
	err := m.writeFile(rel, data)
	m.cfg.journalWrite("cron.d/"+rel, err, func() ([]byte, error) { return m.ReadFile(rel) })
	return err
}

func (m *CronMount) Remove(rel string) error {
	err := m.remove(rel)
	m.cfg.journalDelete("cron.d/"+rel, err)
	return err
}

// Rename is a deletion at the old path and a creation at the new one in the
// history, which has no notion of a move.
func (m *CronMount) Rename(oldRel, newRel string) error {
	err := m.rename(oldRel, newRel)
	m.cfg.journalDelete("cron.d/"+oldRel, err)
	m.cfg.journalWrite("cron.d/"+newRel, err, func() ([]byte, error) { return m.ReadFile(newRel) })
	return err
}

// FileLanguage implements shellcore.LangMount: a cron entry is a comment
// header over a bash pipeline.
func (m *CronMount) FileLanguage(rel string) string { return "bash" }
