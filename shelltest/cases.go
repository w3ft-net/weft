// Package shelltest defines table-driven shell behavior test cases
// that run against both the Go-native CLI path (MountFS + shellcore)
// and the browser WASM path. A go:generate step exports the cases as
// JSON for the frontend vitest harness to consume.
//
// Pure-terminal contract: weft's shell (CLI and browser) is a delegated
// client — it owns no shell semantics. Every command line is shipped to
// the exec backend verbatim via ActionExec; the server (a real pkg/bash
// interpreter over shellfs) lists directories, reads files, and enforces
// read-only mounts. So these cases assert the *dispatch* — that a typed
// line reaches the executor unmangled — not client-rendered output. The
// listing/read/permission semantics are covered server-side in the
// coordinator tests (coordinator/*_test.go) and the live integration
// suite (integration/dirstack_courier_*_test.go).
//
//go:generate env WEFT_FIXTURES_DIR=../../frontend/src/test go test -run TestExportFixtures -count=1
package shelltest

// Case is a single shell-dispatch assertion. The Go test and the TS
// test both exercise the same cases: given Files as the FS state, feed
// Command through shellcore and check it dispatches WantDispatch (the
// pipeline shipped to the executor) verbatim.
type Case struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	// WantDispatch is the pipeline the client must ship to the executor.
	// For the pure-terminal client this equals Command — nothing is
	// rewritten or intercepted client-side.
	WantDispatch string `json:"want_dispatch"`
}

// File is a virtual file present in the test mount before the command
// executes.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	IsDir   bool   `json:"is_dir,omitempty"`
}

// Fixture is a group of cases sharing the same initial FS state.
type Fixture struct {
	Name  string `json:"name"`
	Files []File `json:"files"`
	Cases []Case `json:"cases"`
}

// Fixtures is the complete exported test set. Every command ships
// verbatim — the client is a pure terminal.
var Fixtures = []Fixture{
	{
		Name: "root listing",
		Files: []File{
			{Path: "/bin/hello", Content: "echo hi"},
			{Path: "/tmp/scratch", Content: "temp"},
		},
		Cases: []Case{
			{Name: "ls / ships", Command: "ls /", WantDispatch: "ls /"},
		},
	},
	{
		Name: "bin operations",
		Files: []File{
			{Path: "/bin/hello", Content: "echo hi"},
			{Path: "/bin/check", Content: "#!/usr/bin/env lua\nprint('ok')"},
		},
		Cases: []Case{
			{Name: "ls /bin ships", Command: "ls /bin", WantDispatch: "ls /bin"},
			{Name: "cat /bin/hello ships", Command: "cat /bin/hello", WantDispatch: "cat /bin/hello"},
			{Name: "cat nonexistent ships (server reports the error)", Command: "cat /bin/nope", WantDispatch: "cat /bin/nope"},
		},
	},
	{
		Name: "log queries",
		Files: []File{
			{Path: "/bin/hello", Content: "echo hi"},
		},
		Cases: []Case{
			{Name: "cat /log/<app> ships", Command: "cat /log/myapp.log", WantDispatch: "cat /log/myapp.log"},
			{Name: "ls /log ships", Command: "ls /log", WantDispatch: "ls /log"},
		},
	},
	{
		Name: "tmp operations",
		Files: []File{
			{Path: "/tmp/notes.txt", Content: "remember this"},
		},
		Cases: []Case{
			{Name: "cat /tmp/notes.txt ships", Command: "cat /tmp/notes.txt", WantDispatch: "cat /tmp/notes.txt"},
			{Name: "ls /tmp ships", Command: "ls /tmp", WantDispatch: "ls /tmp"},
		},
	},
	{
		Name: "cron.d operations",
		Files: []File{
			{Path: "/cron.d/heartbeat", Content: "# schedule: */5 * * * *\ncurl localhost/health\n"},
		},
		Cases: []Case{
			{Name: "ls /cron.d ships", Command: "ls /cron.d", WantDispatch: "ls /cron.d"},
			{Name: "cat /cron.d/heartbeat ships", Command: "cat /cron.d/heartbeat", WantDispatch: "cat /cron.d/heartbeat"},
		},
	},
	{
		Name: "rm on read-only mount",
		Files: []File{
			{Path: "/bin/hello", Content: "echo hi"},
		},
		Cases: []Case{
			// rm ships too — the server enforces the read-only mount and
			// reports the error; the client no longer pre-checks.
			{Name: "rm /log/x ships", Command: "rm /log/myapp.log", WantDispatch: "rm /log/myapp.log"},
		},
	},
	{
		Name: "etc subdirectories",
		Files: []File{
			{Path: "/etc/dashboards/ops.yaml", Content: "title: ops"},
			{Path: "/etc/notify.yaml", Content: "aliases: {}"},
		},
		Cases: []Case{
			{Name: "ls /etc ships", Command: "ls /etc", WantDispatch: "ls /etc"},
			{Name: "cat /etc/notify.yaml ships", Command: "cat /etc/notify.yaml", WantDispatch: "cat /etc/notify.yaml"},
		},
	},
}
