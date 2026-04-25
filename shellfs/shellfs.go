// Package shellfs implements the warp bash.FS interface backed by PostgreSQL
// via the store. It presents a virtual filesystem to the weft shell:
//
//	/log/*.log   — read-only pseudofiles backed by shard queries
//	/bin/*       — user-defined bash functions
//	/cron.d/*    — scheduled watch definitions
//	/etc/notify.conf — notification channel configuration
//	/tmp/*       — per-customer scratch files (persistent for 48h)
package shellfs

import (
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/kevlarcon/warp/pkg/bash"
	"github.com/kevlarcon/weft/store"
)

// Compile-time interface check.
var _ bash.FS = (*ShellFS)(nil)

// ShellFS implements bash.FS for the weft shell.
type ShellFS struct {
	store        *store.Store
	customerID   uint
	customerName string     // for usage queries
	tier         store.Tier // subscription tier
	logApps      []string   // app names for /log/ directory listing
	userID       string     // for permission checks (empty = owner/no filtering)
	isOwner      bool       // owners bypass namespace permissions

	// noticeQueue, when non-nil, is signaled by /tmp write paths the
	// first time the user touches /tmp in a session — the queue
	// itself decides whether to surface the warning, dedup across
	// the session, and persist the seen flag. Optional; nil = no
	// notices delivered (used by tests and non-user contexts).
	noticeQueue NoticeSink
}

// NoticeSink is the narrow interface ShellFS uses to signal events
// that should produce a one-time notice for the invoking user. The
// coordinator's NoticeQueue type satisfies this; tests can substitute
// a stub. The interface lives in shellfs (not coordinator) so
// shellfs has no dependency on coordinator — coordinator already
// imports shellfs, going the other way would create a cycle.
type NoticeSink interface {
	Maybe(noticeKey string)
}

// NoticeKeyTmpExpiry is the notice key for the 48h /tmp expiry
// warning. Defined here (rather than in coordinator) because shellfs
// is what triggers the notice — the registry entry that maps key →
// title/body lives in coordinator/notices.go.
const NoticeKeyTmpExpiry = "tmp_48h_expiry"

// New creates a ShellFS for the given customer.
func New(s *store.Store, customerID uint, logApps []string, opts ...Option) *ShellFS {
	fs := &ShellFS{
		store:      s,
		customerID: customerID,
		tier:       store.TierTrial,
		logApps:    logApps,
	}
	for _, o := range opts {
		o(fs)
	}
	return fs
}

// Option configures a ShellFS.
type Option func(*ShellFS)

// WithTier sets the subscription tier for limit enforcement.
func WithTier(tier store.Tier) Option {
	return func(f *ShellFS) { f.tier = tier }
}

// WithCustomerName sets the customer name for usage queries.
func WithCustomerName(name string) Option {
	return func(f *ShellFS) { f.customerName = name }
}

// WithUser sets the user ID for permission checks.
func WithUser(userID string, isOwner bool) Option {
	return func(f *ShellFS) {
		f.userID = userID
		f.isOwner = isOwner
	}
}

// WithNoticeQueue wires a notice sink the FS signals on /tmp writes.
// The coordinator's NoticeQueue is the production implementation;
// tests can pass a stub.
func WithNoticeQueue(q NoticeSink) Option {
	return func(f *ShellFS) { f.noticeQueue = q }
}

// ReadFile returns the content of a virtual file.
func (f *ShellFS) ReadFile(path string) ([]byte, error) {
	dir, name := splitPath(path)

	switch dir {
	case "bin":
		fn, err := f.store.GetShellFunction(f.customerID, name)
		if err != nil {
			return nil, fmt.Errorf("%s: no such file", path)
		}
		// Reconstruct shebang for display so vi/cat shows the original.
		if fn.Language == "lua" {
			return []byte("#!/usr/bin/env lua\n" + fn.Source), nil
		}
		return []byte(fn.Source), nil

	case "cron.d":
		w, err := f.store.GetCronWatch(f.customerID, name)
		if err != nil {
			return nil, fmt.Errorf("%s: no such file", path)
		}
		return []byte(serializeCron(w)), nil

	case "etc":
		if name == "notify.conf" {
			nc, err := f.store.GetNotifyConfig(f.customerID)
			if err != nil {
				return []byte(""), nil // empty config is valid
			}
			return []byte(serializeNotifyConfig(nc)), nil
		}
		if name == "usage" {
			return []byte(f.buildUsageReport()), nil
		}
		return nil, fmt.Errorf("%s: no such file", path)

	case "log":
		// Log pseudofiles are read-only markers. The planner intercepts
		// `cat <app>.log` before it reaches the FS.
		app := strings.TrimSuffix(name, ".log")
		if f.hasLogApp(app) {
			ns := namespaceFromApp(app)
			if !f.canReadNamespace(ns) {
				return nil, fmt.Errorf("%s: permission denied", path)
			}
			return []byte{}, nil
		}
		return nil, fmt.Errorf("%s: no such file", path)

	case "tmp":
		data, err := f.store.ReadTmpFile(f.customerID, path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return data, nil
	}

	return nil, fmt.Errorf("%s: no such file or directory", path)
}

// canReadNamespace checks if the user has read access to a namespace.
// Owners and users with no userID (legacy/dev) always have access.
func (f *ShellFS) canReadNamespace(ns string) bool {
	if f.isOwner || f.userID == "" {
		return true
	}
	// Look up the namespace ID, then check permission.
	nsObj, err := f.store.FindOrCreateNamespace(f.customerID, ns)
	if err != nil {
		return false
	}
	perm, err := f.store.GetNamespacePermission(f.userID, nsObj.ID)
	if err != nil {
		return false // no permission row = no access
	}
	return perm.CanRead
}

// canManageInNamespace checks a specific permission on a namespace.
func (f *ShellFS) canManageInNamespace(ns, perm string) bool {
	if f.isOwner || f.userID == "" {
		return true
	}
	nsObj, err := f.store.FindOrCreateNamespace(f.customerID, ns)
	if err != nil {
		return false
	}
	p, err := f.store.GetNamespacePermission(f.userID, nsObj.ID)
	if err != nil {
		return false
	}
	switch perm {
	case "watches":
		return p.CanManageWatches
	case "functions":
		return p.CanManageFunctions
	case "tokens":
		return p.CanCreateTokens
	case "integrations":
		return p.CanManageIntegrations
	default:
		return false
	}
}

// namespaceFromApp extracts the namespace from an app name.
// "weft/coordinator" → "weft", "vimgym" → "vimgym"
func namespaceFromApp(app string) string {
	if idx := strings.Index(app, "/"); idx >= 0 {
		return app[:idx]
	}
	return app
}

// hasLogApp checks if an app name exists in the log apps list.
// Matches both exact ("weft/coordinator") and namespace-only ("weft" matches "weft/coordinator").
func (f *ShellFS) hasLogApp(app string) bool {
	for _, a := range f.logApps {
		if a == app {
			return true
		}
	}
	return false
}

// logNamespaces returns unique namespace prefixes from logApps.
func (f *ShellFS) logNamespaces() []string {
	seen := map[string]bool{}
	var nss []string
	for _, a := range f.logApps {
		ns := a
		if idx := strings.Index(a, "/"); idx >= 0 {
			ns = a[:idx]
		}
		if !seen[ns] {
			seen[ns] = true
			nss = append(nss, ns)
		}
	}
	return nss
}

// logAppsInNamespace returns sub-app names within a namespace.
// For "weft" with apps ["weft/coordinator", "weft/shard"], returns
// ["coordinator", "shard"]. Skips entries with an env segment —
// those are listed under logEnvsInNamespace and surfaced as env
// subdirectories in the directory listing. See
// docs/plans/ingest-dimensions.md.
func (f *ShellFS) logAppsInNamespace(ns string) []string {
	prefix := ns + "/"
	var apps []string
	for _, a := range f.logApps {
		if !strings.HasPrefix(a, prefix) {
			continue
		}
		rest := strings.TrimPrefix(a, prefix)
		if strings.Contains(rest, "/") {
			continue // env-bearing entry; env tier handles it
		}
		apps = append(apps, rest)
	}
	return apps
}

// logEnvsInNamespace returns the distinct env values for env-bearing
// apps in a namespace. Pairs with logAppsInNamespace to drive the
// mixed listing at /log/<ns>/ — env-less apps appear as files,
// env-bearing apps appear as env subdirectories.
func (f *ShellFS) logEnvsInNamespace(ns string) []string {
	prefix := ns + "/"
	seen := map[string]bool{}
	var envs []string
	for _, a := range f.logApps {
		if !strings.HasPrefix(a, prefix) {
			continue
		}
		rest := strings.TrimPrefix(a, prefix)
		i := strings.Index(rest, "/")
		if i < 0 {
			continue
		}
		env := rest[:i]
		if !seen[env] {
			seen[env] = true
			envs = append(envs, env)
		}
	}
	return envs
}

// logAppsInNamespaceEnv returns sub-app names for a (ns, env) pair.
// For "weft"/"prod" with apps ["weft/prod/api", "weft/prod/worker"],
// returns ["api", "worker"].
func (f *ShellFS) logAppsInNamespaceEnv(ns, env string) []string {
	prefix := ns + "/" + env + "/"
	var apps []string
	for _, a := range f.logApps {
		if strings.HasPrefix(a, prefix) {
			apps = append(apps, strings.TrimPrefix(a, prefix))
		}
	}
	return apps
}

// WriteFile creates or updates a virtual file.
func (f *ShellFS) WriteFile(path string, data []byte) error {
	dir, name := splitPath(path)

	switch dir {
	case "bin":
		return f.store.SaveShellFunction(f.customerID, f.userID, name, string(data))

	case "cron.d":
		schedule, pipeline, err := parseCron(string(data))
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}

		// Enforce tier limits.
		maxWatches := store.WatchLimit(f.tier)
		if maxWatches == 0 {
			return fmt.Errorf("watches are not available on the trial plan")
		}

		// Check count limit only for new watches.
		if _, err := f.store.GetCronWatch(f.customerID, name); err != nil {
			count, err := f.store.CountCronWatches(f.customerID)
			if err != nil {
				return fmt.Errorf("failed to check watch count: %w", err)
			}
			if count >= maxWatches {
				return fmt.Errorf("watch limit reached (%d watches on %s plan)", maxWatches, f.tier)
			}
		}

		// Check minimum interval.
		minInterval := store.MinWatchInterval(f.tier)
		interval := store.ParseScheduleInterval(schedule)
		if interval < minInterval {
			return fmt.Errorf("minimum schedule interval for %s plan is %s", f.tier, minInterval)
		}

		return f.store.SaveCronWatch(f.customerID, f.userID, name, schedule, pipeline)

	case "etc":
		if name == "notify.conf" {
			validated, err := parseNotifyConfig(string(data))
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			return f.store.SaveNotifyAliases(f.customerID, validated)
		}
		return fmt.Errorf("%s: read-only", path)

	case "log":
		return fmt.Errorf("%s: read-only", path)

	case "tmp":
		if err := f.store.WriteTmpFile(f.customerID, f.tier, f.userID, path, data); err != nil {
			return err
		}
		f.noteFirstTmpWrite()
		return nil
	}

	return fmt.Errorf("%s: no such directory", path)
}

// Stat returns file info for a path.
func (f *ShellFS) Stat(path string) (fs.FileInfo, error) {
	dir, name := splitPath(path)

	// Top-level directory.
	if name == "" {
		switch dir {
		case "bin", "cron.d", "log", "etc", "tmp":
			return &vfsFileInfo{name: dir, dir: true}, nil
		}
		// Root directory.
		if dir == "" {
			return &vfsFileInfo{name: "/", dir: true}, nil
		}
		return nil, fmt.Errorf("%s: no such file or directory", path)
	}

	// Check if file exists.
	switch dir {
	case "bin":
		if fn, err := f.store.GetShellFunction(f.customerID, name); err == nil {
			return &vfsFileInfo{name: name, size: int64(len(fn.Source)), mtime: fn.UpdatedAt}, nil
		}
	case "cron.d":
		if w, err := f.store.GetCronWatch(f.customerID, name); err == nil {
			return &vfsFileInfo{name: name, size: int64(len(serializeCron(w))), mtime: w.UpdatedAt}, nil
		}
	case "etc":
		if name == "notify.conf" {
			if nc, err := f.store.GetNotifyConfig(f.customerID); err == nil {
				return &vfsFileInfo{name: name, size: int64(len(nc.AliasesYAML)), mtime: nc.UpdatedAt}, nil
			}
			// No config row yet — still a legitimate (empty) file.
			return &vfsFileInfo{name: name}, nil
		}
		if name == "usage" {
			return &vfsFileInfo{name: name, mtime: time.Now()}, nil
		}
	case "log":
		// Could be a namespace dir (name="weft") or a file (name="vimgym.log" or "weft/coordinator.log").
		if !strings.HasSuffix(name, ".log") {
			for _, ns := range f.logNamespaces() {
				if ns == name {
					if !f.canReadNamespace(ns) {
						return nil, fmt.Errorf("%s: permission denied", path)
					}
					return &vfsFileInfo{name: name, dir: true}, nil
				}
			}
		} else {
			app := strings.TrimSuffix(name, ".log")
			if f.hasLogApp(app) {
				ns := namespaceFromApp(app)
				if !f.canReadNamespace(ns) {
					return nil, fmt.Errorf("%s: permission denied", path)
				}
				return &vfsFileInfo{name: name}, nil
			}
		}
	case "tmp":
		tf, err := f.store.GetTmpFile(f.customerID, path)
		if err != nil {
			return nil, fmt.Errorf("%s: no such file or directory", path)
		}
		return &vfsFileInfo{
			name:  lastSegment(name),
			dir:   tf.IsDir,
			size:  tf.ByteSize,
			mtime: tf.UpdatedAt,
		}, nil
	}

	return nil, fmt.Errorf("%s: no such file or directory", path)
}

// ReadDir lists entries in a directory.
func (f *ShellFS) ReadDir(path string) ([]fs.DirEntry, error) {
	dir, _ := splitPath(path)

	// Root directory.
	if dir == "" || path == "/" {
		return []fs.DirEntry{
			&vfsDirEntry{name: "bin", dir: true},
			&vfsDirEntry{name: "cron.d", dir: true},
			&vfsDirEntry{name: "etc", dir: true},
			&vfsDirEntry{name: "log", dir: true},
			&vfsDirEntry{name: "tmp", dir: true},
		}, nil
	}

	switch dir {
	case "bin":
		fns, err := f.store.ListShellFunctions(f.customerID)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(fns))
		for _, fn := range fns {
			if fn.CreatedByUserID != nil {
				ids = append(ids, *fn.CreatedByUserID)
			}
		}
		owners := f.resolveOwners(ids)
		entries := make([]fs.DirEntry, len(fns))
		for i, fn := range fns {
			owner := ""
			if fn.CreatedByUserID != nil {
				owner = owners[*fn.CreatedByUserID]
			}
			entries[i] = &vfsDirEntry{
				name:  fn.Name,
				size:  int64(len(fn.Source)),
				mtime: fn.UpdatedAt,
				owner: owner,
			}
		}
		return entries, nil

	case "cron.d":
		watches, err := f.store.ListCronWatches(f.customerID)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(watches))
		for _, w := range watches {
			if w.CreatedByUserID != nil {
				ids = append(ids, *w.CreatedByUserID)
			}
		}
		owners := f.resolveOwners(ids)
		entries := make([]fs.DirEntry, len(watches))
		for i, w := range watches {
			owner := ""
			if w.CreatedByUserID != nil {
				owner = owners[*w.CreatedByUserID]
			}
			entries[i] = &vfsDirEntry{
				name:  w.Name,
				size:  int64(len(serializeCron(&w))),
				mtime: w.UpdatedAt,
				owner: owner,
			}
		}
		return entries, nil

	case "etc":
		var notifySize int64
		var notifyMtime time.Time
		if nc, err := f.store.GetNotifyConfig(f.customerID); err == nil {
			notifySize = int64(len(nc.AliasesYAML))
			notifyMtime = nc.UpdatedAt
		}
		return []fs.DirEntry{
			&vfsDirEntry{name: "notify.conf", size: notifySize, mtime: notifyMtime},
			&vfsDirEntry{name: "usage", mtime: time.Now()},
		}, nil

	case "tmp":
		rows, err := f.store.ListTmpFiles(f.customerID, path)
		if err != nil {
			return nil, err
		}
		owners := f.resolveOwners(collectCreatorIDs(rows))
		entries := make([]fs.DirEntry, len(rows))
		for i, r := range rows {
			owner := ""
			if r.CreatedByUserID != nil {
				owner = owners[*r.CreatedByUserID]
			}
			entries[i] = &vfsDirEntry{
				name:  lastSegment(r.Path),
				dir:   r.IsDir,
				size:  r.ByteSize,
				mtime: r.UpdatedAt,
				owner: owner,
			}
		}
		return entries, nil

	case "log":
		_, logName := splitPath(path)
		if logName == "" {
			// /log/ — show namespace directories (and flat apps as .log files).
			nss := f.logNamespaces()
			var entries []fs.DirEntry
			for _, ns := range nss {
				if !f.canReadNamespace(ns) {
					continue
				}
				if len(f.logAppsInNamespace(ns)) > 0 || len(f.logEnvsInNamespace(ns)) > 0 {
					entries = append(entries, &vfsDirEntry{name: ns, dir: true})
				} else {
					entries = append(entries, &vfsDirEntry{name: ns + ".log"})
				}
			}
			return entries, nil
		}
		// Either "/log/<ns>/" or "/log/<ns>/<env>/". Three-segment paths
		// peel the env off and list the apps within that env. See
		// docs/plans/ingest-dimensions.md for the layout rationale.
		segs := strings.Split(logName, "/")
		nsName := segs[0]
		if !f.canReadNamespace(nsName) {
			return nil, fmt.Errorf("%s: permission denied", path)
		}
		switch len(segs) {
		case 1:
			// /log/<ns>/ — mix of <env>/ subdirs (env-bearing apps) and
			// <app>.log files (env-less apps).
			var entries []fs.DirEntry
			for _, env := range f.logEnvsInNamespace(nsName) {
				entries = append(entries, &vfsDirEntry{name: env, dir: true})
			}
			for _, app := range f.logAppsInNamespace(nsName) {
				entries = append(entries, &vfsDirEntry{name: app + ".log"})
			}
			if len(entries) > 0 {
				return entries, nil
			}
		case 2:
			// /log/<ns>/<env>/ — apps within this (ns, env) pair.
			env := segs[1]
			subApps := f.logAppsInNamespaceEnv(nsName, env)
			if len(subApps) > 0 {
				entries := make([]fs.DirEntry, len(subApps))
				for i, app := range subApps {
					entries[i] = &vfsDirEntry{name: app + ".log"}
				}
				return entries, nil
			}
		}
	}

	return nil, fmt.Errorf("%s: no such directory", path)
}

// Remove deletes a virtual file.
func (f *ShellFS) Remove(path string) error {
	dir, name := splitPath(path)

	switch dir {
	case "bin":
		return f.store.DeleteShellFunction(f.customerID, name)
	case "cron.d":
		return f.store.DeleteCronWatch(f.customerID, name)
	case "log", "etc":
		return fmt.Errorf("%s: operation not permitted", path)
	case "tmp":
		// rm doesn't trigger the first-write notice — deletion
		// shouldn't surprise a user who's never created anything,
		// and a stray rm against a missing path returns an error
		// we don't want to clutter with the courtesy line.
		return f.store.DeleteTmpFile(f.customerID, path)
	}

	return fmt.Errorf("%s: no such file or directory", path)
}

// AppendFile implements bash.AppendFS for /tmp. Other namespaces
// (/bin, /cron.d, /etc) have no sensible append semantics — their
// contents are structured YAML or script text, not line-oriented
// data — so append there falls back to WriteFile's semantics via
// warp's read-modify-write path.
func (f *ShellFS) AppendFile(path string, data []byte) error {
	dir, _ := splitPath(path)
	if dir != "tmp" {
		// Fall back to the canonical read-modify-write the warp
		// interpreter would use: that's what `>>` means on these
		// read-then-replace namespaces.
		existing, _ := f.ReadFile(path)
		return f.WriteFile(path, append(existing, data...))
	}
	if err := f.store.AppendTmpFile(f.customerID, f.tier, f.userID, path, data); err != nil {
		return err
	}
	f.noteFirstTmpWrite()
	return nil
}

// Mkdir implements bash.MkdirFS. Only meaningful inside /tmp — the
// other namespaces are flat by construction.
func (f *ShellFS) Mkdir(path string) error {
	dir, _ := splitPath(path)
	if dir != "tmp" {
		return fmt.Errorf("%s: mkdir not supported here", path)
	}
	if err := f.store.Mkdir(f.customerID, f.userID, path); err != nil {
		return err
	}
	f.noteFirstTmpWrite()
	return nil
}

// RemoveAll implements bash.RemoveAllFS for /tmp (rm -rf). Other
// namespaces fall back to the standard Remove behaviour.
func (f *ShellFS) RemoveAll(path string) error {
	dir, _ := splitPath(path)
	if dir == "tmp" {
		return f.store.DeleteTmpSubtree(f.customerID, path)
	}
	return f.Remove(path)
}

// CopyFile implements bash.CopyFS for /tmp. Cross-namespace copies
// (e.g., /tmp/x → /bin/y) are rejected — /bin runs writes through a
// syntax validator, and copying arbitrary text into it would route
// around that. Users who really mean to do this can `cat src > dst`,
// which goes through the destination namespace's WriteFile.
func (f *ShellFS) CopyFile(src, dst string) error {
	srcDir, _ := splitPath(src)
	dstDir, _ := splitPath(dst)
	if srcDir != "tmp" || dstDir != "tmp" {
		return fmt.Errorf("cp: cross-namespace copy not supported (use `cat %s > %s` instead)", src, dst)
	}
	if err := f.store.CopyTmpFile(f.customerID, f.tier, f.userID, src, dst); err != nil {
		return err
	}
	f.noteFirstTmpWrite()
	return nil
}

// Rename implements bash.RenameFS for /tmp. Same cross-namespace
// prohibition as CopyFile.
func (f *ShellFS) Rename(oldPath, newPath string) error {
	oldDir, _ := splitPath(oldPath)
	newDir, _ := splitPath(newPath)
	if oldDir != "tmp" || newDir != "tmp" {
		return fmt.Errorf("mv: cross-namespace rename not supported")
	}
	if err := f.store.RenameTmpFile(f.customerID, oldPath, newPath); err != nil {
		return err
	}
	f.noteFirstTmpWrite()
	return nil
}

// Touch implements bash.TouchFS. Only meaningful in /tmp where
// updated_at drives the 48h retention clock; on other namespaces
// returns an error since they have no expiry to extend.
func (f *ShellFS) Touch(path string) error {
	dir, _ := splitPath(path)
	if dir != "tmp" {
		return fmt.Errorf("touch: only supported in /tmp")
	}
	if err := f.store.TouchTmpFile(f.customerID, f.tier, f.userID, path); err != nil {
		return err
	}
	f.noteFirstTmpWrite()
	return nil
}

// resolveOwners batch-resolves a list of user IDs to display names
// for `ls -al`'s owner column. Single store round-trip, deduped.
// Display name follows the fallback chain: User.Name → email
// local-part → first 8 chars of UUID. Truncated to 12 chars.
//
// Empty IDs (NULL CreatedByUserID rows) are skipped — they render
// as `-` in the long-form output via the empty-owner branch in
// builtinLs.
func (f *ShellFS) resolveOwners(ids []string) map[string]string {
	out := map[string]string{}
	if len(ids) == 0 {
		return out
	}
	// Dedup before the SQL hit.
	uniq := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		uniq = append(uniq, id)
	}
	users, err := f.store.GetUsersByIDs(uniq)
	if err != nil {
		return out
	}
	for id, u := range users {
		out[id] = ownerDisplayName(u)
	}
	// Any IDs we couldn't resolve get the UUID-prefix fallback so
	// `ls -al` still shows *something* informative.
	for _, id := range uniq {
		if _, ok := out[id]; !ok {
			out[id] = truncateOwner(id, 12)
		}
	}
	return out
}

// collectCreatorIDs pulls the non-nil CreatedByUserID values from a
// slice of TmpFile rows. Saves an inline loop at every ReadDir call
// site.
func collectCreatorIDs(rows []store.TmpFile) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.CreatedByUserID != nil {
			out = append(out, *r.CreatedByUserID)
		}
	}
	return out
}

// ownerDisplayName picks the friendliest available label for a user.
// Fallback chain: User.Name → email local-part → first 8 chars of
// UUID. Truncated to 12 chars to keep ls columns readable.
func ownerDisplayName(u store.User) string {
	if u.Name != "" {
		return truncateOwner(u.Name, 12)
	}
	if at := strings.IndexByte(u.Email, '@'); at > 0 {
		return truncateOwner(u.Email[:at], 12)
	}
	return truncateOwner(u.ID, 12)
}

func truncateOwner(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// lastSegment returns the final path component. "incidents/a" → "a",
// "foo" → "foo". Used by Stat/ReadDir on /tmp where store paths are
// full sub-paths but fs.DirEntry.Name() is the basename.
func lastSegment(p string) string {
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		return p[idx+1:]
	}
	return p
}

// noteFirstTmpWrite triggers the 48h-expiry notice on the request's
// notice queue. Idempotent per session and per user — the queue
// handles the bookkeeping; ShellFS just signals "this is a
// notice-worthy event." Called after every successful /tmp mutation;
// failing writes don't trigger.
//
// When `noticeQueue` is nil (legacy or no-user contexts), this is a
// no-op. The queue itself decides whether the user has already seen
// this notice and whether to enqueue it for delivery.
func (f *ShellFS) noteFirstTmpWrite() {
	if f.noticeQueue == nil {
		return
	}
	f.noticeQueue.Maybe(NoticeKeyTmpExpiry)
}

// FileLanguage returns the scripting language for a /bin/ file.
// Implements bash.LangFS.
func (f *ShellFS) FileLanguage(path string) string {
	dir, name := splitPath(path)
	if dir != "bin" {
		return ""
	}
	fn, err := f.store.GetShellFunction(f.customerID, name)
	if err != nil {
		return ""
	}
	return fn.Language
}

// ── Path helpers ────────────────────────────────────────────

// splitPath splits "/bin/check_errors" into ("bin", "check_errors").
// "/log" returns ("log", ""). "/" returns ("", "").
func splitPath(path string) (dir, name string) {
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		return "", ""
	}
	if idx := strings.Index(path, "/"); idx >= 0 {
		return path[:idx], path[idx+1:]
	}
	return path, ""
}

// ── Cron file serialization ─────────────────────────────────

func serializeCron(w *store.CronWatch) string {
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

// parseCron extracts the `# schedule:` directive and the shell pipeline body.
// Alert destinations are no longer watch-level metadata — they come from
// `alert --notify=<alias>` stages inside the pipeline itself.
func parseCron(content string) (schedule, pipeline string, err error) {
	var pipelineLines []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# schedule:") {
			schedule = strings.TrimSpace(strings.TrimPrefix(trimmed, "# schedule:"))
		} else if strings.HasPrefix(trimmed, "# enabled:") {
			// Ignored on write — enabled/disabled via separate API.
		} else if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			pipelineLines = append(pipelineLines, line)
		}
	}
	pipeline = strings.Join(pipelineLines, "\n")
	if schedule == "" {
		err = fmt.Errorf("missing '# schedule:' directive")
	}
	if pipeline == "" {
		err = fmt.Errorf("missing pipeline")
	}
	return
}

// ── Notify config serialization ─────────────────────────────
//
// /etc/notify.conf is a YAML map of alias → per-service targets. Example:
//
//     ops:
//       slack: ["#ops-alerts"]
//       pagerduty:
//         - name: ops-daytime
//           severity: error
//
// Cron watches reference aliases via `notify: <alias-name>`. Slack credentials
// and PagerDuty integration keys live in the database (managed through the
// Account → Integrations UI); notify.conf carries routing only, never secrets.

func serializeNotifyConfig(nc *store.NotifyConfig) string {
	if nc == nil {
		return ""
	}
	return nc.AliasesYAML
}

// parseNotifyConfig validates the YAML alias document. Returns the raw input
// on success so the store layer persists exactly what the user wrote.
func parseNotifyConfig(content string) (string, error) {
	if _, err := store.ParseNotifyAliases(content); err != nil {
		return "", err
	}
	return content, nil
}

// ── Usage report ───────────────────────────────────────────────

func (f *ShellFS) buildUsageReport() string {
	var b strings.Builder

	now := time.Now()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)

	b.WriteString("# weft usage report\n")
	fmt.Fprintf(&b, "# period: %s — %s\n", periodStart.Format("2006-01-02"), periodEnd.Format("2006-01-02"))
	fmt.Fprintf(&b, "# tier: %s\n\n", f.tier)

	// Actual ingestion.
	bytesIngested, err := f.store.QueryUsage(f.customerName, periodStart, periodEnd)
	if err != nil {
		bytesIngested = 0
	}

	tierLimitBytes := store.VolumeAllowance(f.tier)

	pct := 0.0
	if tierLimitBytes > 0 {
		pct = float64(bytesIngested) / float64(tierLimitBytes) * 100
	}

	fmt.Fprintf(&b, "ingested:  %s / %s (%.1f%%)\n",
		humanBytes(bytesIngested), humanBytes(tierLimitBytes), pct)

	// Metrics forecast.
	forecast, err := f.store.ForecastMetricsUsage(f.customerID)
	if err == nil && forecast.AgentsWithMetrics > 0 {
		fmt.Fprintf(&b, "metrics:   ~%s/mo projected (%d agent%s)\n",
			humanBytes(forecast.ProjectedBytesMonth),
			forecast.AgentsWithMetrics, plural(forecast.AgentsWithMetrics))

		totalProjected := bytesIngested + forecast.ProjectedBytesMonth
		totalPct := float64(totalProjected) / float64(tierLimitBytes) * 100
		fmt.Fprintf(&b, "total:     ~%s projected (%.1f%% of limit)\n",
			humanBytes(totalProjected), totalPct)
	}

	return b.String()
}

func humanBytes(b int64) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%.2f GB", float64(b)/(1024*1024*1024))
	case b >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ── fs.FileInfo / fs.DirEntry implementations ───────────────

type vfsFileInfo struct {
	name  string
	dir   bool
	size  int64
	mtime time.Time
	owner string // resolved display name; "" = no attribution → renders as `-`
}

func (fi *vfsFileInfo) Name() string       { return fi.name }
func (fi *vfsFileInfo) Size() int64        { return fi.size }
func (fi *vfsFileInfo) Mode() fs.FileMode  { if fi.dir { return fs.ModeDir | 0755 }; return 0644 }
func (fi *vfsFileInfo) ModTime() time.Time { return fi.mtime }
func (fi *vfsFileInfo) IsDir() bool        { return fi.dir }

// Sys returns an interface satisfying bash.OwnerInfo. The warp `ls -l`
// builtin reads it to render an owner column.
func (fi *vfsFileInfo) Sys() any { return ownerOnly{fi.owner} }

// Owner implements bash.OwnerInfo. Empty string renders as `-` in
// the long-form ls output.
func (o ownerOnly) Owner() string { return o.name }

// ownerOnly is the type Sys() returns — wraps just the owner string
// so callers don't accidentally cast Sys() to something larger.
type ownerOnly struct{ name string }

type vfsDirEntry struct {
	name  string
	dir   bool
	size  int64
	mtime time.Time
	owner string
}

func (de *vfsDirEntry) Name() string      { return de.name }
func (de *vfsDirEntry) IsDir() bool       { return de.dir }
func (de *vfsDirEntry) Type() fs.FileMode { if de.dir { return fs.ModeDir }; return 0 }
func (de *vfsDirEntry) Info() (fs.FileInfo, error) {
	return &vfsFileInfo{name: de.name, dir: de.dir, size: de.size, mtime: de.mtime, owner: de.owner}, nil
}
