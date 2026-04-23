// Package shellfs implements the warp bash.FS interface backed by PostgreSQL
// via the store. It presents a virtual filesystem to the weft shell:
//
//	/log/*.log   — read-only pseudofiles backed by shard queries
//	/bin/*       — user-defined bash functions
//	/cron.d/*    — scheduled watch definitions
//	/etc/notify.conf — notification channel configuration
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
	userID       string   // for permission checks (empty = owner/no filtering)
	isOwner      bool     // owners bypass namespace permissions
}

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
// For "weft" with apps ["weft/coordinator", "weft/shard"], returns ["coordinator", "shard"].
func (f *ShellFS) logAppsInNamespace(ns string) []string {
	prefix := ns + "/"
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
		return f.store.SaveShellFunction(f.customerID, name, string(data))

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

		return f.store.SaveCronWatch(f.customerID, name, schedule, pipeline)

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
	}

	return fmt.Errorf("%s: no such directory", path)
}

// Stat returns file info for a path.
func (f *ShellFS) Stat(path string) (fs.FileInfo, error) {
	dir, name := splitPath(path)

	// Top-level directory.
	if name == "" {
		switch dir {
		case "bin", "cron.d", "log", "etc":
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
		if _, err := f.store.GetShellFunction(f.customerID, name); err == nil {
			return &vfsFileInfo{name: name}, nil
		}
	case "cron.d":
		if _, err := f.store.GetCronWatch(f.customerID, name); err == nil {
			return &vfsFileInfo{name: name}, nil
		}
	case "etc":
		if name == "notify.conf" || name == "usage" {
			return &vfsFileInfo{name: name}, nil
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
		}, nil
	}

	switch dir {
	case "bin":
		fns, err := f.store.ListShellFunctions(f.customerID)
		if err != nil {
			return nil, err
		}
		entries := make([]fs.DirEntry, len(fns))
		for i, fn := range fns {
			entries[i] = &vfsDirEntry{name: fn.Name}
		}
		return entries, nil

	case "cron.d":
		watches, err := f.store.ListCronWatches(f.customerID)
		if err != nil {
			return nil, err
		}
		entries := make([]fs.DirEntry, len(watches))
		for i, w := range watches {
			entries[i] = &vfsDirEntry{name: w.Name}
		}
		return entries, nil

	case "etc":
		return []fs.DirEntry{
			&vfsDirEntry{name: "notify.conf"},
			&vfsDirEntry{name: "usage"},
		}, nil

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
				subApps := f.logAppsInNamespace(ns)
				if len(subApps) > 0 {
					entries = append(entries, &vfsDirEntry{name: ns, dir: true})
				} else {
					entries = append(entries, &vfsDirEntry{name: ns + ".log"})
				}
			}
			return entries, nil
		}
		// /log/<namespace>/ — show sub-apps as .log files.
		if !f.canReadNamespace(logName) {
			return nil, fmt.Errorf("%s: permission denied", path)
		}
		subApps := f.logAppsInNamespace(logName)
		if len(subApps) > 0 {
			entries := make([]fs.DirEntry, len(subApps))
			for i, app := range subApps {
				entries[i] = &vfsDirEntry{name: app + ".log"}
			}
			return entries, nil
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
	}

	return fmt.Errorf("%s: no such file or directory", path)
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

	var tierLimitBytes int64
	switch f.tier {
	case store.TierSwatch:
		tierLimitBytes = 10 * 1024 * 1024 * 1024
	case store.TierYard:
		tierLimitBytes = 100 * 1024 * 1024 * 1024
	case store.TierBolt:
		tierLimitBytes = 1024 * 1024 * 1024 * 1024
	default:
		tierLimitBytes = 1 * 1024 * 1024 * 1024
	}

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
	name string
	dir  bool
}

func (fi *vfsFileInfo) Name() string        { return fi.name }
func (fi *vfsFileInfo) Size() int64         { return 0 }
func (fi *vfsFileInfo) Mode() fs.FileMode   { if fi.dir { return fs.ModeDir | 0755 }; return 0644 }
func (fi *vfsFileInfo) ModTime() time.Time  { return time.Time{} }
func (fi *vfsFileInfo) IsDir() bool         { return fi.dir }
func (fi *vfsFileInfo) Sys() any            { return nil }

type vfsDirEntry struct {
	name string
	dir  bool
}

func (de *vfsDirEntry) Name() string               { return de.name }
func (de *vfsDirEntry) IsDir() bool                { return de.dir }
func (de *vfsDirEntry) Type() fs.FileMode          { if de.dir { return fs.ModeDir }; return 0 }
func (de *vfsDirEntry) Info() (fs.FileInfo, error)  { return &vfsFileInfo{name: de.name, dir: de.dir}, nil }
