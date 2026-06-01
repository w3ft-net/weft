package runner

import (
	"reflect"
	"testing"

	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/weft/shell"
)

// TestRunQuery_CachesReturnedDirStack proves the embedder half of the
// dirstack courier: runQuery ships the core's cached dirstack to Query
// and writes the server's FinalDirStack back into the core, so a shipped
// `cd`/`pushd` persists to the prompt and the next request. The stub
// Query stands in for the HTTP round-trip (which TestDirstackCourier_HTTP
// covers live); here we pin that the client sends the cwd and caches the
// reply.
func TestRunQuery_CachesReturnedDirStack(t *testing.T) {
	var sentDirStack []string
	core := shellcore.NewShellCore(nil, 80, 24, func([]byte) {}, func(shellcore.Action) {})

	r := &runner{
		core: core,
		shell: &shell.Shell{
			Range: "1d..",
			Query: func(pipeline, rangeSpec string, dirStack []string) (*shell.QueryResult, error) {
				sentDirStack = dirStack
				// Server moved the cwd (e.g. the pipeline was `cd /log/acme`).
				return &shell.QueryResult{FinalDirStack: []string{"/log/acme"}}, nil
			},
		},
	}

	// Fresh core starts at "/".
	if got := core.DirStack(); !reflect.DeepEqual(got, []string{"/"}) {
		t.Fatalf("initial DirStack = %v, want [/]", got)
	}

	r.runQuery("cd /log/acme")

	// The client shipped its cached cwd ("/") with the request...
	if !reflect.DeepEqual(sentDirStack, []string{"/"}) {
		t.Errorf("Query received dirStack %v, want the cached [/]", sentDirStack)
	}
	// ...and cached the server's returned stack so the next prompt/request
	// tracks /log/acme.
	if got := core.DirStack(); !reflect.DeepEqual(got, []string{"/log/acme"}) {
		t.Errorf("after runQuery, DirStack = %v, want [/log/acme] (server's FinalDirStack not cached)", got)
	}
}

// TestRunQuery_NoDirStackLeavesCwd: when the server returns no dirstack
// (a non-cd pipeline, or the in-process dev shell), the cached cwd is
// left untouched — a missing trailer must not blank the prompt.
func TestRunQuery_NoDirStackLeavesCwd(t *testing.T) {
	core := shellcore.NewShellCore(nil, 80, 24, func([]byte) {}, func(shellcore.Action) {})
	core.SetDirStack([]string{"/log/acme"})

	r := &runner{
		core: core,
		shell: &shell.Shell{
			Range: "1d..",
			Query: func(pipeline, rangeSpec string, dirStack []string) (*shell.QueryResult, error) {
				return &shell.QueryResult{Text: "hi\n"}, nil // no FinalDirStack
			},
		},
	}

	r.runQuery("cat api.log")

	if got := core.DirStack(); !reflect.DeepEqual(got, []string{"/log/acme"}) {
		t.Errorf("DirStack = %v, want [/log/acme] preserved (nil trailer must not reset cwd)", got)
	}
}
