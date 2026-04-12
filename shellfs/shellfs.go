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
	customerName string   // for usage queries
	tier         string   // subscription tier (trial, swatch, yard, bolt)
	logApps      []string // app names for /log/ directory listing
}

// New creates a ShellFS for the given customer.
func New(s *store.Store, customerID uint, logApps []string, opts ...Option) *ShellFS {
	fs := &ShellFS{
		store:      s,
		customerID: customerID,
		tier:       "trial",
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
func WithTier(tier string) Option {
	return func(f *ShellFS) { f.tier = tier }
}

// WithCustomerName sets the customer name for usage queries.
func WithCustomerName(name string) Option {
	return func(f *ShellFS) { f.customerName = name }
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
		// Log pseudofiles are read-only markers. The actual content comes
		// from the query pipeline, not from ReadFile. Return an empty
		// placeholder so stat/cat don't error — the planner intercepts
		// `cat <app>.log` before it reaches the FS.
		app := strings.TrimSuffix(name, ".log")
		for _, a := range f.logApps {
			if a == app {
				return []byte{}, nil
			}
		}
		return nil, fmt.Errorf("%s: no such file", path)
	}

	return nil, fmt.Errorf("%s: no such file or directory", path)
}

// WriteFile creates or updates a virtual file.
func (f *ShellFS) WriteFile(path string, data []byte) error {
	dir, name := splitPath(path)

	switch dir {
	case "bin":
		return f.store.SaveShellFunction(f.customerID, name, string(data))

	case "cron.d":
		schedule, pipeline, notifyDest, err := parseCron(string(data))
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

		return f.store.SaveCronWatch(f.customerID, name, schedule, pipeline, notifyDest)

	case "etc":
		if name == "notify.conf" {
			slackWebhook := parseNotifyConfig(string(data))
			return f.store.SaveNotifyConfig(f.customerID, slackWebhook)
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
		app := strings.TrimSuffix(name, ".log")
		for _, a := range f.logApps {
			if a == app {
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
		entries := make([]fs.DirEntry, len(f.logApps))
		for i, app := range f.logApps {
			entries[i] = &vfsDirEntry{name: app + ".log"}
		}
		return entries, nil
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
	if w.NotifyDest != "" {
		fmt.Fprintf(&b, "# notify: %s\n", w.NotifyDest)
	}
	if !w.Enabled {
		b.WriteString("# enabled: false\n")
	}
	b.WriteString(w.Pipeline)
	if !strings.HasSuffix(w.Pipeline, "\n") {
		b.WriteByte('\n')
	}
	return b.String()
}

func parseCron(content string) (schedule, pipeline, notifyDest string, err error) {
	var pipelineLines []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# schedule:") {
			schedule = strings.TrimSpace(strings.TrimPrefix(trimmed, "# schedule:"))
		} else if strings.HasPrefix(trimmed, "# notify:") {
			notifyDest = strings.TrimSpace(strings.TrimPrefix(trimmed, "# notify:"))
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

func serializeNotifyConfig(nc *store.NotifyConfig) string {
	var b strings.Builder
	if nc.SlackWebhook != "" {
		fmt.Fprintf(&b, "slack_webhook: %s\n", nc.SlackWebhook)
	}
	return b.String()
}

func parseNotifyConfig(content string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "slack_webhook:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "slack_webhook:"))
		}
	}
	return ""
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
	case "swatch":
		tierLimitBytes = 10 * 1024 * 1024 * 1024
	case "yard":
		tierLimitBytes = 100 * 1024 * 1024 * 1024
	case "bolt":
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
