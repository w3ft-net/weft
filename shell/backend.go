package shell

import (
	"fmt"
	"strings"

	"github.com/kevlarcon/weft/chart"
)

// WatchData holds the deserialized fields of a cron watch.
type WatchData struct {
	Schedule string
	Pipeline string
	Enabled  bool
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

	GetNotifyConfig() (string, error)
	SaveNotifyConfig(content string) error
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
}

// NewRemoteFS creates an FS backed by the given transport.
func NewRemoteFS(backend Backend, apps []string) *RemoteFS {
	return &RemoteFS{backend: backend, apps: apps}
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
		if name == "notify.conf" {
			yaml, err := f.backend.GetNotifyConfig()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			return []byte(yaml), nil
		}
	}

	return nil, fmt.Errorf("%s: not found", path)
}

func (f *RemoteFS) WriteFile(path string, data []byte) error {
	dir, name := splitFSPath(normFSPath(path))

	switch dir {
	case "bin":
		return f.backend.SaveFunction(name, string(data))
	case "cron.d":
		schedule, pipeline := parseCronFileContent(string(data))
		return f.backend.SaveWatch(name, &WatchData{
			Schedule: schedule,
			Pipeline: pipeline,
			Enabled:  true,
		})
	case "etc":
		if name == "notify.conf" {
			return f.backend.SaveNotifyConfig(string(data))
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
		names, err := f.backend.ListFunctions()
		if err != nil {
			return []string{}, nil
		}
		return names, nil

	case "/cron.d":
		names, err := f.backend.ListWatches()
		if err != nil {
			return []string{}, nil
		}
		return names, nil

	case "/etc":
		return []string{"notify.conf", "usage"}, nil
	}

	// /log/<namespace>/[<env>/] — see docs/plans/ingest-dimensions.md.
	// At /log/<ns>/ we emit a mix:
	//   - <env>/ subdirectories (deduped) for env-bearing apps,
	//   - <app>.log files for env-less apps in that namespace.
	// At /log/<ns>/<env>/ we emit just <app>.log files filtered by env.
	if strings.HasPrefix(p, "/log/") {
		segs := strings.Split(strings.TrimPrefix(strings.TrimSuffix(p, "/"), "/log/"), "/")
		switch len(segs) {
		case 1:
			ns := segs[0]
			return f.entriesForNamespace(ns), nil
		case 2:
			ns, env := segs[0], segs[1]
			return f.entriesForNamespaceEnv(ns, env), nil
		}
	}

	return nil, fmt.Errorf("%s: no such directory", path)
}

// entriesForNamespace returns the mixed listing for /log/<ns>/. App
// entries known with no env produce ".log" files; app entries known
// only with envs produce env-named subdirectories (deduped). Result
// can contain both shapes when a namespace has a mix.
func (f *RemoteFS) entriesForNamespace(ns string) []string {
	prefix := ns + "/"
	envSeen := map[string]bool{}
	var entries []string
	for _, name := range f.apps {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		if i := strings.Index(rest, "/"); i >= 0 {
			env := rest[:i]
			if !envSeen[env] {
				envSeen[env] = true
				entries = append(entries, env+"/")
			}
		} else {
			entries = append(entries, rest+".log")
		}
	}
	return entries
}

// entriesForNamespaceEnv returns the apps known under a specific
// (ns, env) pair, formatted as "<app>.log".
func (f *RemoteFS) entriesForNamespaceEnv(ns, env string) []string {
	prefix := ns + "/" + env + "/"
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
		return f.backend.DeleteFunction(name)
	case "cron.d":
		return f.backend.DeleteWatch(name)
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
