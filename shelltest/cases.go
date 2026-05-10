// Package shelltest defines table-driven shell behavior test cases
// that run against both the Go-native CLI path (MountFS + shellcore)
// and the browser WASM path. A go:generate step exports the cases as
// JSON for the frontend vitest harness to consume.
//
//go:generate go test -run TestExportFixtures -count=1
package shelltest

// Case is a single shell behavior assertion. The Go test and the TS
// test both exercise the same cases: given Files as the FS state,
// feed Command through shellcore, and check the output matches Want.
type Case struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Want    string `json:"want"`
	// WantErr, when non-empty, means the command should produce this
	// error message instead of Want output.
	WantErr string `json:"want_err,omitempty"`
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

// Fixtures is the complete exported test set.
var Fixtures = []Fixture{
	{
		Name: "root listing",
		Files: []File{
			{Path: "/bin/hello", Content: "echo hi"},
			{Path: "/tmp/scratch", Content: "temp"},
		},
		Cases: []Case{
			{
				Name:    "ls / shows mount prefixes",
				Command: "ls /",
				Want:    "bin/\ncron.d/\netc/\nlog/\ntmp/\n",
			},
		},
	},
	{
		Name: "bin operations",
		Files: []File{
			{Path: "/bin/hello", Content: "echo hi"},
			{Path: "/bin/check", Content: "#!/usr/bin/env lua\nprint('ok')"},
		},
		Cases: []Case{
			{
				Name:    "ls /bin lists functions",
				Command: "ls /bin",
				Want:    "check\nhello\n",
			},
			{
				Name:    "cat /bin/hello returns content",
				Command: "cat /bin/hello",
				Want:    "echo hi\n",
			},
			{
				Name:    "cat nonexistent file errors",
				Command: "cat /bin/nope",
				WantErr: "no such file",
			},
		},
	},
	{
		Name: "log path classification",
		Files: []File{
			{Path: "/bin/hello", Content: "echo hi"},
		},
		Cases: []Case{
			{
				Name:    "cat /log path dispatches query",
				Command: "cat /log/myapp.log",
				Want:    "__QUERY_DISPATCHED__",
			},
			{
				Name:    "ls /log lists apps",
				Command: "ls /log",
				Want:    "myapp.log\n",
			},
		},
	},
	{
		Name: "tmp operations",
		Files: []File{
			{Path: "/tmp/notes.txt", Content: "remember this"},
		},
		Cases: []Case{
			{
				Name:    "cat /tmp/notes.txt",
				Command: "cat /tmp/notes.txt",
				Want:    "remember this\n",
			},
			{
				Name:    "ls /tmp",
				Command: "ls /tmp",
				Want:    "notes.txt\n",
			},
		},
	},
	{
		Name: "cron.d operations",
		Files: []File{
			{Path: "/cron.d/heartbeat", Content: "# schedule: */5 * * * *\ncurl localhost/health\n"},
		},
		Cases: []Case{
			{
				Name:    "ls /cron.d",
				Command: "ls /cron.d",
				Want:    "heartbeat\n",
			},
			{
				Name:    "cat /cron.d/heartbeat",
				Command: "cat /cron.d/heartbeat",
				Want:    "# schedule: */5 * * * *\ncurl localhost/health\n",
			},
		},
	},
	{
		Name: "rm on read-only mount",
		Files: []File{
			{Path: "/bin/hello", Content: "echo hi"},
		},
		Cases: []Case{
			{
				Name:    "rm /log/x fails read-only",
				Command: "rm /log/myapp.log",
				WantErr: "read-only",
			},
		},
	},
	{
		Name: "etc subdirectories",
		Files: []File{
			{Path: "/etc/dashboards/ops.yaml", Content: "title: ops"},
			{Path: "/etc/notify.yaml", Content: "aliases: {}"},
		},
		Cases: []Case{
			{
				Name:    "ls /etc shows subdirs and files",
				Command: "ls /etc",
				Want:    "dashboards/\nnotify.yaml\n",
			},
			{
				Name:    "cat /etc/notify.yaml",
				Command: "cat /etc/notify.yaml",
				Want:    "aliases: {}\n",
			},
		},
	},
}
