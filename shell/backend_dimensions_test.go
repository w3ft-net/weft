package shell

import (
	"reflect"
	"sort"
	"testing"
)

// stubBackend satisfies Backend with empty stubs — these tests exercise
// only the FS-side directory logic, not transport.
type stubBackend struct{}

func (stubBackend) ListFunctions() ([]string, error)             { return nil, nil }
func (stubBackend) GetFunction(name string) (string, error)      { return "", nil }
func (stubBackend) SaveFunction(name, source string) error       { return nil }
func (stubBackend) DeleteFunction(name string) error             { return nil }
func (stubBackend) ListWatches() ([]string, error)               { return nil, nil }
func (stubBackend) GetWatch(name string) (*WatchData, error)     { return nil, nil }
func (stubBackend) SaveWatch(name string, w *WatchData) error    { return nil }
func (stubBackend) DeleteWatch(name string) error                { return nil }
func (stubBackend) ListDashboards() ([]string, error)            { return nil, nil }
func (stubBackend) GetDashboard(name string) (string, error)     { return "", nil }
func (stubBackend) SaveDashboard(name, body string) error        { return nil }
func (stubBackend) DeleteDashboard(name string) error            { return nil }
func (stubBackend) GetNotifyConfig() (string, error)             { return "", nil }
func (stubBackend) SaveNotifyConfig(content string) error        { return nil }

// TestRemoteFS_LogPathDiscovery verifies the 2-or-3-level /log/
// listing layout: env-bearing entries in a namespace surface as
// subdirectories, env-less entries as <app>.log files, and
// /log/<ns>/<env>/ lists just the apps under that env. See
// docs/plans/ingest-dimensions.md.
func TestRemoteFS_LogPathDiscovery(t *testing.T) {
	apps := []string{
		// env-less
		"weft/coordinator",
		"weft/api",
		// env-bearing under "myco" namespace
		"myco/prod/api",
		"myco/prod/worker",
		"myco/staging/api",
		// pure env-less namespace
		"vimgym/web",
	}
	fs := NewRemoteFS(stubBackend{}, apps)

	// /log/ — namespace listing.
	got, err := fs.ReadDir("/log")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"myco/", "vimgym/", "weft/"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("/log listing: got %v, want %v", got, want)
	}

	// /log/weft/ — pure env-less namespace shows two .log files.
	got, err = fs.ReadDir("/log/weft")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want = []string{"api.log", "coordinator.log"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("/log/weft listing: got %v, want %v", got, want)
	}

	// /log/myco/ — pure env-bearing namespace shows env subdirs only.
	got, err = fs.ReadDir("/log/myco")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want = []string{"prod/", "staging/"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("/log/myco listing: got %v, want %v", got, want)
	}

	// /log/myco/prod/ — apps under that env.
	got, err = fs.ReadDir("/log/myco/prod")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want = []string{"api.log", "worker.log"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("/log/myco/prod listing: got %v, want %v", got, want)
	}

	// /log/myco/staging/ — single app.
	got, err = fs.ReadDir("/log/myco/staging")
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"api.log"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("/log/myco/staging listing: got %v, want %v", got, want)
	}
}

// TestRemoteFS_LogPathMixedNamespace verifies a single namespace
// can hold both env-less and env-bearing entries — the listing
// shows the mix.
func TestRemoteFS_LogPathMixedNamespace(t *testing.T) {
	apps := []string{
		"weft/legacy",        // env-less, in same namespace as env-bearing
		"weft/prod/coordinator",
		"weft/staging/coordinator",
	}
	fs := NewRemoteFS(stubBackend{}, apps)

	got, err := fs.ReadDir("/log/weft")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{"legacy.log", "prod/", "staging/"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("/log/weft mixed listing: got %v, want %v", got, want)
	}
}
