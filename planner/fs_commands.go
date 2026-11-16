package planner

// fsCommands are the shell commands that browse the pseudofile tree
// rather than read log content. Handed a /log/ path they are directory
// navigation (`cd /log/acme/prod`, `ls /log/acme`, `tree /log/acme`), which
// plain warp+shellfs serve from the LogMount — not log queries, so they
// must never be pulled into the planner or rejected with the "pipeline
// must start with cat, head, or tail" error.
//
// That error exists for content readers (grep, awk, jq, sed, ...) given a
// /log/ file directly, which would otherwise read the LogMount and return
// no rows with no error. It is an explicit allowlist, not a path-shape
// rule, so a newly added command that reads a log path is rejected loudly
// until someone decides it belongs here.
var fsCommands = map[string]bool{
	"cd":    true,
	"pushd": true,
	"popd":  true,
	"dirs":  true,
	"pwd":   true,
	"ls":    true,
	"tree":  true,
	"find":  true,
	"stat":  true,
	"du":    true,
}

// IsFilesystemCommand reports whether name is a filesystem-navigation
// command that must fall through to plain warp execution when handed a
// /log/ path.
func IsFilesystemCommand(name string) bool { return fsCommands[name] }
