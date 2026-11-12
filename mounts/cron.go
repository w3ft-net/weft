package mounts

import (
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"strings"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/planner"
	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
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
	_, err := m.cfg.Store.GetNamespace(m.cfg.OrgID, first)
	isNS := err == nil
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
		if !m.cfg.allowedIn(tenancy.ActionManageCron, p.ns) {
			return denyNamespaceWrite("/cron.d/"+p.ns, p.ns)
		}
	default:
		// Org-level crons are on their way out: an existing one stays editable
		// by an owner until it is moved, but no new one can be made.
		if _, err := m.get(rel); err != nil {
			return fmt.Errorf("/cron.d/%s: %w (a cron belongs to a namespace; make it at /cron.d/<namespace>/%s)", p.name, fs.ErrPermission, p.name)
		}
		if !m.cfg.allowed(tenancy.ActionPublishOrgWide) {
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
	ns, err := m.cfg.Store.GetNamespace(m.cfg.OrgID, p.ns)
	if err != nil {
		return 0, err
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
	watches, err := m.cfg.Store.ListCronWatches(m.cfg.OrgID)
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
	var here []store.CronWatch
	for _, w := range watches {
		if (w.NamespaceID == nil && home == 0) || (w.NamespaceID != nil && *w.NamespaceID == home) {
			here = append(here, w)
		}
	}
	ids := make([]string, 0, len(here))
	for _, w := range here {
		if w.CreatedByUserID != nil {
			ids = append(ids, *w.CreatedByUserID)
		}
	}
	owners := resolveOwners(m.cfg.Store, ids)
	for _, w := range here {
		owner := ""
		if w.CreatedByUserID != nil {
			owner = owners[*w.CreatedByUserID]
		}
		entries = append(entries, &vfsDirEntry{
			name:  w.Name,
			size:  int64(len(SerializeCron(&w))),
			mtime: w.UpdatedAt,
			owner: owner,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

// get finds the cron at rel.
func (m *CronMount) get(rel string) (*store.CronWatch, error) {
	p, err := m.resolve(rel)
	if err != nil || p.name == "" {
		return nil, fs.ErrNotExist
	}
	home, err := m.home(p)
	if err != nil {
		return nil, fs.ErrNotExist
	}
	w, err := m.cfg.Store.GetCronWatch(m.cfg.OrgID, home, p.name)
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
	return []byte(SerializeCron(w)), nil
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
	return &vfsFileInfo{name: p.name, size: int64(len(SerializeCron(w))), mtime: w.UpdatedAt}, nil
}

func (m *CronMount) WriteFile(rel string, data []byte) error {
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
	schedule, pipeline, enabled, err := ParseCron(string(data))
	if err != nil {
		return fmt.Errorf("/cron.d/%s: %w", rel, err)
	}

	maxWatches := store.WatchLimit(m.cfg.Tier)
	if maxWatches == 0 {
		return &PlanLimitError{"crons are not available on the trial plan"}
	}

	// A cron that is new, or being turned back on, takes a slot; disabled
	// crons are free.
	if existing, err := m.cfg.Store.GetCronWatch(m.cfg.OrgID, home, p.name); enabled && (err != nil || !existing.Enabled) {
		count, err := m.cfg.Store.CountCronWatches(m.cfg.OrgID)
		if err != nil {
			return fmt.Errorf("failed to check cron count: %w", err)
		}
		if count >= maxWatches {
			return &PlanLimitError{fmt.Sprintf("cron limit reached (%d crons on %s plan)", maxWatches, m.cfg.Tier)}
		}
	}

	minInterval := store.MinWatchInterval(m.cfg.Tier)
	interval, err := store.ValidateSchedule(schedule)
	if err != nil {
		return err
	}
	if interval < minInterval {
		return &PlanLimitError{fmt.Sprintf("minimum schedule interval for %s plan is %s", m.cfg.Tier, minInterval)}
	}

	if p.ns != "" {
		// A namespace's cron has that namespace's authority and no more.
		err = m.cfg.authorizeHomedCronReads(p.ns, pipeline)
	} else {
		err = m.cfg.authorizeCronReads(pipeline)
	}
	if err != nil {
		return err
	}

	// Lower at save time so the fire path can skip preflight + Plan.
	// LowerForStorage returns nil on any failure (e.g. unsupported
	// /bin/ pipeline) — the save still succeeds, and the fire path
	// falls back to inline Plan() for those.
	resolver := cronBinResolver{store: m.cfg.Store, orgID: m.cfg.OrgID}
	loweredJSON := planner.LowerForStorage(m.cfg.ctx(), pipeline, shape.Set{m.cfg.partition().Coord()}, string(m.cfg.Tier), resolver)

	return m.cfg.Store.SaveCronWatch(m.cfg.OrgID, home, m.cfg.UserID, p.name, schedule, pipeline, enabled, loweredJSON)
}

// cronBinResolver adapts the shell mount's store + customer scope to
// planner.BinResolver. Mirrors the adapter in api/handlers.go and
// coordinator/service/server.go — kept duplicated rather than shared
// because the type is tiny and pulling a helper package together for
// three identical 10-line adapters is unjustified.
type cronBinResolver struct {
	store *store.Store
	orgID store.OrgID
}

func (r cronBinResolver) Source(name string) (string, string, bool) {
	fn, err := r.store.GetShellFunction(r.orgID, name)
	if err != nil || fn == nil {
		return "", "", false
	}
	return fn.Source, planner.HashBinSource(fn.Source), true
}

func (m *CronMount) Remove(rel string) error {
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
	return m.cfg.Store.DeleteCronWatch(m.cfg.OrgID, home, p.name)
}

// Rename moves a cron to another home, or renames it, as the same row, so its
// history and results come with it. MountFS has already authorized a write at
// both paths.
func (m *CronMount) Rename(oldRel, newRel string) error {
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
	if err := m.cfg.Store.MoveCronWatch(m.cfg.OrgID, fromHome, from.name, toHome, to.name); err != nil {
		return err
	}
	// Dashboards name crons, and a tile naming the old path no longer finds it.
	if from.ns == "" {
		if refs := m.dashboardsNaming(from.name); len(refs) > 0 {
			return store.NewWarning("moved /cron.d/%s to /cron.d/%s, but dashboards %s still name it %q; point their tiles at %q", from.name, newRel, strings.Join(refs, ", "), from.name, newRel)
		}
	}
	return nil
}

// dashboardsNaming lists the dashboards with a tile on the org-level cron name.
func (m *CronMount) dashboardsNaming(name string) []string {
	ds, err := m.cfg.Store.ListDashboards(m.cfg.OrgID)
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range ds {
		spec, err := store.ParseDashboardYAML(d.YAMLBody)
		if err != nil {
			continue
		}
		for _, sec := range spec.Sections {
			for _, t := range sec.Tiles {
				if t.Watch == name {
					out = append(out, d.Name)
					goto next
				}
			}
		}
	next:
	}
	return out
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
	return m.cfg.Store.AmendCronWatchMessage(m.cfg.OrgID, home, m.cfg.UserID, p.name, message)
}

// --- Cron serialization (canonical, single source of truth) ---

func SerializeCron(w *store.CronWatch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# schedule: %s\n", w.Schedule)
	if !w.Enabled {
		b.WriteString("# enabled: false\n")
	}
	b.WriteString(w.Pipeline)
	if !strings.HasSuffix(w.Pipeline, "\n") {
		b.WriteByte('\n')
	}
	return b.String()
}

// ParseCron splits a /cron.d file into its parts. Enabled is true unless the
// file says `# enabled: false`: dropping the comment is how a user turns a
// benched or paused cron back on. The `# value:`/`# tag:`/`# type:` headers
// stay in the pipeline, where store.CompileWatchPipeline reads them.
func ParseCron(content string) (schedule, pipeline string, enabled bool, err error) {
	enabled = true
	var pipelineLines []string
	hasCommand := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# value:") || strings.HasPrefix(trimmed, "# tag:") || strings.HasPrefix(trimmed, "# type:") {
			pipelineLines = append(pipelineLines, trimmed)
		} else if strings.HasPrefix(trimmed, "# schedule:") {
			schedule = strings.TrimSpace(strings.TrimPrefix(trimmed, "# schedule:"))
		} else if strings.HasPrefix(trimmed, "# enabled:") {
			switch v := strings.TrimSpace(strings.TrimPrefix(trimmed, "# enabled:")); v {
			case "true":
			case "false":
				enabled = false
			default:
				return "", "", false, fmt.Errorf("'# enabled:' must be true or false, got %q", v)
			}
		} else if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			pipelineLines = append(pipelineLines, line)
			hasCommand = true
		}
	}
	pipeline = strings.Join(pipelineLines, "\n")
	if schedule == "" {
		err = fmt.Errorf("missing '# schedule:' directive")
	}
	if !hasCommand {
		err = fmt.Errorf("missing pipeline")
	}
	return
}

// FileLanguage implements shellcore.LangMount: a cron entry is a comment
// header over a bash pipeline.
func (m *CronMount) FileLanguage(rel string) string { return "bash" }
