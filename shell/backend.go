package shell

import (
	"fmt"
	"strings"
	"sync"

	"github.com/kevlarcon/weft/chart"
	"github.com/kevlarcon/weft/store"
)

// WatchData holds the deserialized fields of a cron watch.
type WatchData struct {
	Schedule string
	Pipeline string
	Enabled  bool
	Title    string // optional human-friendly name from "# title: ..." comment
}

// Backend is the transport layer for coordinator storage operations.
// Implementations handle gRPC, HTTP, or in-memory storage; the RemoteFS
// uses this interface to avoid duplicating directory-tree logic.
type Backend interface {
	ListFunctions() ([]string, error)
	GetFunction(name string) (string, error)
	SaveFunction(name, source string) error
	DeleteFunction(name string) error

	ListWatches() ([]string, error)
	GetWatch(name string) (*WatchData, error)
	SaveWatch(name string, w *WatchData) error
	DeleteWatch(name string) error

	ListDashboards() ([]string, error)
	GetDashboard(name string) (string, error)
	SaveDashboard(name, body string) error
	DeleteDashboard(name string) error

	GetNotifyConfig() (string, error)
	SaveNotifyConfig(content string) error

	// GetFile reads the bytes of a virtual file the specialized
	// methods don't cover (e.g. /etc/usage, which is generated rather
	// than stored). RemoteFS falls back to this for any /etc path
	// that isn't notify.yaml or under /bin / /cron.d / /log.
	GetFile(path string) ([]byte, error)
}

// WatchResultsBackend is optionally implemented by backends that can
// return historical cron watch results.
type WatchResultsBackend interface {
	GetWatchResults(name string, limit int) ([]chart.WatchResultValue, error)
}

// RemoteFS implements shellcore.FS by combining a Backend (transport)
// with directory-tree logic and an app list (for /log/).
type RemoteFS struct {
	backend Backend
	apps    []string

	mu        sync.RWMutex
	dirCache  map[string][]string          // path → entries
	specCache map[string]*store.DashboardSpec // dashboard name → parsed spec
}

// NewRemoteFS creates an FS backed by the given transport.
func NewRemoteFS(backend Backend, apps []string) *RemoteFS {
	return &RemoteFS{
		backend:   backend,
		apps:      apps,
		dirCache:  make(map[string][]string),
		specCache: make(map[string]*store.DashboardSpec),
	}
}

// Prefetch populates the directory cache for paths that require network
// calls. Call from a goroutine during startup (e.g. while the logo
// dissolves) so tab completion is instant from the first keypress.
func (f *RemoteFS) Prefetch() {
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		if names, err := f.backend.ListFunctions(); err == nil {
			f.setDirCache("/bin", names)
		}
	}()
	go func() {
		defer wg.Done()
		if names, err := f.backend.ListWatches(); err == nil {
			f.setDirCache("/cron.d", names)
		}
	}()
	go func() {
		defer wg.Done()
		if names, err := f.backend.ListDashboards(); err == nil {
			for i, n := range names {
				names[i] = n + ".yaml"
			}
			f.setDirCache("/etc/dashboards", names)
		}
	}()
	wg.Wait()
}

func (f *RemoteFS) setDirCache(path string, entries []string) {
	f.mu.Lock()
	f.dirCache[path] = entries
	f.mu.Unlock()
}

func (f *RemoteFS) getDirCache(path string) ([]string, bool) {
	f.mu.RLock()
	entries, ok := f.dirCache[path]
	f.mu.RUnlock()
	return entries, ok
}

func (f *RemoteFS) invalidateDirCache(path string) {
	f.mu.Lock()
	delete(f.dirCache, path)
	f.mu.Unlock()
}

func (f *RemoteFS) ReadFile(path string) ([]byte, error) {
	dir, name := splitFSPath(normFSPath(path))

	switch dir {
	case "bin":
		src, err := f.backend.GetFunction(name)
		if err != nil {
			return nil, fmt.Errorf("%s: not found", path)
		}
		return []byte(src), nil

	case "cron.d":
		w, err := f.backend.GetWatch(name)
		if err != nil {
			return nil, fmt.Errorf("%s: not found", path)
		}
		return []byte(serializeCronFile(w)), nil

	case "etc":
		if name == "notify.yaml" {
			yaml, err := f.backend.GetNotifyConfig()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			return []byte(yaml), nil
		}
		if strings.HasPrefix(name, "dashboards/") {
			dName := strings.TrimPrefix(name, "dashboards/")
			dName = strings.TrimSuffix(dName, ".yaml")
			body, err := f.backend.GetDashboard(dName)
			if err != nil {
				return nil, fmt.Errorf("%s: not found", path)
			}
			return []byte(body), nil
		}
	}

	if data, err := f.backend.GetFile(path); err == nil {
		return data, nil
	}
	return nil, fmt.Errorf("%s: not found", path)
}

func (f *RemoteFS) WriteFile(path string, data []byte) error {
	dir, name := splitFSPath(normFSPath(path))

	switch dir {
	case "bin":
		if err := f.backend.SaveFunction(name, string(data)); err != nil {
			return err
		}
		f.invalidateDirCache("/bin")
		return nil
	case "cron.d":
		schedule, pipeline := parseCronFileContent(string(data))
		if err := f.backend.SaveWatch(name, &WatchData{
			Schedule: schedule,
			Pipeline: pipeline,
			Enabled:  true,
		}); err != nil {
			return err
		}
		f.invalidateDirCache("/cron.d")
		return nil
	case "etc":
		if name == "notify.yaml" {
			return f.backend.SaveNotifyConfig(string(data))
		}
		if strings.HasPrefix(name, "dashboards/") {
			dName := strings.TrimPrefix(name, "dashboards/")
			dName = strings.TrimSuffix(dName, ".yaml")
			if err := f.backend.SaveDashboard(dName, string(data)); err != nil {
				return err
			}
			f.invalidateDirCache("/etc/dashboards")
			if spec, err := store.ParseDashboardYAML(string(data)); err == nil {
				f.mu.Lock()
				f.specCache[dName] = spec
				f.mu.Unlock()
			}
			return nil
		}
	}

	return fmt.Errorf("%s: read-only", path)
}

func (f *RemoteFS) ReadDir(path string) ([]string, error) {
	p := normFSPath(path)

	switch p {
	case "/":
		return []string{"bin/", "cron.d/", "log/", "etc/"}, nil

	case "/log":
		return f.logEntries(), nil

	case "/bin":
		if cached, ok := f.getDirCache("/bin"); ok {
			return cached, nil
		}
		names, err := f.backend.ListFunctions()
		if err != nil {
			return []string{}, nil
		}
		f.setDirCache("/bin", names)
		return names, nil

	case "/cron.d":
		if cached, ok := f.getDirCache("/cron.d"); ok {
			return cached, nil
		}
		names, err := f.backend.ListWatches()
		if err != nil {
			return []string{}, nil
		}
		f.setDirCache("/cron.d", names)
		return names, nil

	case "/etc":
		return []string{"dashboards/", "notify.yaml", "usage"}, nil

	case "/etc/dashboards":
		if cached, ok := f.getDirCache("/etc/dashboards"); ok {
			return cached, nil
		}
		names, err := f.backend.ListDashboards()
		if err != nil {
			return []string{}, nil
		}
		for i, n := range names {
			names[i] = n + ".yaml"
		}
		f.setDirCache("/etc/dashboards", names)
		return names, nil
	}

	// /log/<namespace>/[<service>/] — see docs/plans/ingest-dimensions.md.
	// At /log/<ns>/ we emit a mix:
	//   - <service>/ subdirectories (deduped) for apps with a path component
	//     after the namespace (e.g., weft/coordinator/runtime),
	//   - <app>.log files for plain apps in that namespace.
	// At /log/<ns>/<service>/ we emit just <kind>.log files for the apps
	// whose names start with <ns>/<service>/.
	if strings.HasPrefix(p, "/log/") {
		segs := strings.Split(strings.TrimPrefix(strings.TrimSuffix(p, "/"), "/log/"), "/")
		switch len(segs) {
		case 1:
			ns := segs[0]
			return f.entriesForNamespace(ns), nil
		case 2:
			ns, service := segs[0], segs[1]
			return f.entriesForNamespaceService(ns, service), nil
		}
	}

	return nil, fmt.Errorf("%s: no such directory", path)
}

// entriesForNamespace returns the mixed listing for /log/<ns>/. Plain
// apps (no extra path component) appear as ".log" files; apps with a
// path component after the namespace (e.g. <ns>/coordinator/runtime)
// produce a deduped <service>/ subdirectory entry.
func (f *RemoteFS) entriesForNamespace(ns string) []string {
	prefix := ns + "/"
	serviceSeen := map[string]bool{}
	var entries []string
	for _, name := range f.apps {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		if i := strings.Index(rest, "/"); i >= 0 {
			service := rest[:i]
			if !serviceSeen[service] {
				serviceSeen[service] = true
				entries = append(entries, service+"/")
			}
		} else {
			entries = append(entries, rest+".log")
		}
	}
	return entries
}

// entriesForNamespaceService returns the apps known under a specific
// (ns, service) pair, formatted as "<kind>.log".
func (f *RemoteFS) entriesForNamespaceService(ns, service string) []string {
	prefix := ns + "/" + service + "/"
	var entries []string
	for _, name := range f.apps {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		entries = append(entries, strings.TrimPrefix(name, prefix)+".log")
	}
	return entries
}

func (f *RemoteFS) Remove(path string) error {
	dir, name := splitFSPath(normFSPath(path))

	switch dir {
	case "bin":
		if err := f.backend.DeleteFunction(name); err != nil {
			return err
		}
		f.invalidateDirCache("/bin")
		return nil
	case "cron.d":
		if err := f.backend.DeleteWatch(name); err != nil {
			return err
		}
		f.invalidateDirCache("/cron.d")
		return nil
	case "etc":
		if strings.HasPrefix(name, "dashboards/") {
			dName := strings.TrimPrefix(name, "dashboards/")
			dName = strings.TrimSuffix(dName, ".yaml")
			if err := f.backend.DeleteDashboard(dName); err != nil {
				return err
			}
			f.invalidateDirCache("/etc/dashboards")
			f.mu.Lock()
			delete(f.specCache, dName)
			f.mu.Unlock()
			return nil
		}
	}

	return fmt.Errorf("%s: operation not permitted", path)
}

// GetWatchResults delegates to the backend if it supports it.
func (f *RemoteFS) GetWatchResults(name string, limit int) ([]chart.WatchResultValue, error) {
	if wrb, ok := f.backend.(WatchResultsBackend); ok {
		return wrb.GetWatchResults(name, limit)
	}
	return nil, fmt.Errorf("watch results not supported")
}

// GetDashboard delegates to the backend and caches the parsed spec.
func (f *RemoteFS) GetDashboard(name string) (string, error) {
	body, err := f.backend.GetDashboard(name)
	if err != nil {
		return "", err
	}
	if spec, perr := store.ParseDashboardYAML(body); perr == nil {
		f.mu.Lock()
		f.specCache[name] = spec
		f.mu.Unlock()
	}
	return body, nil
}

// GetDashboardSpec returns the cached parsed spec for a dashboard, or
// fetches + parses it if not cached. Used by the dashboard view to
// render the frame without re-fetching YAML.
func (f *RemoteFS) GetDashboardSpec(name string) (*store.DashboardSpec, error) {
	f.mu.RLock()
	spec, ok := f.specCache[name]
	f.mu.RUnlock()
	if ok {
		return spec, nil
	}
	body, err := f.GetDashboard(name)
	if err != nil {
		return nil, err
	}
	return store.ParseDashboardYAML(body)
}

// ListDashboards delegates to the backend.
func (f *RemoteFS) ListDashboards() ([]string, error) {
	return f.backend.ListDashboards()
}

// logEntries groups apps by namespace prefix for /log/ listing.
func (f *RemoteFS) logEntries() []string {
	seen := map[string]bool{}
	var entries []string
	for _, app := range f.apps {
		if idx := strings.Index(app, "/"); idx >= 0 {
			ns := app[:idx]
			if !seen[ns] {
				seen[ns] = true
				entries = append(entries, ns+"/")
			}
		} else {
			entries = append(entries, app+".log")
		}
	}
	return entries
}

// ── Shared helpers ─────────────────────────────────────────

func normFSPath(path string) string {
	if path == "" || path == "." {
		return "/"
	}
	if path[0] != '/' {
		path = "/" + path
	}
	if len(path) > 1 && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	return path
}

func splitFSPath(path string) (dir, name string) {
	path = strings.TrimPrefix(path, "/")
	if idx := strings.Index(path, "/"); idx >= 0 {
		return path[:idx], path[idx+1:]
	}
	return path, ""
}

func serializeCronFile(w *WatchData) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# schedule: %s\n", w.Schedule)
	if w.Title != "" {
		fmt.Fprintf(&b, "# %s\n", w.Title)
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

func parseCronFileContent(content string) (schedule, pipeline string) {
	var pipelineLines []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# schedule:") {
			schedule = strings.TrimSpace(strings.TrimPrefix(trimmed, "# schedule:"))
		} else if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			pipelineLines = append(pipelineLines, line)
		}
	}
	pipeline = strings.Join(pipelineLines, "\n")
	return
}
