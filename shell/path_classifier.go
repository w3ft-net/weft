package shell

import (
	"strings"

	"github.com/kevlarcon/warp/pkg/shellcore"
)

// PathClassifier maps weft's virtual FS hierarchy to PathKinds the
// shellcore stock builtins (cat, head, tail) consult to decide
// whether to FS-read or route through StartQuery. This is the
// classifier that USED to live in shellcore as the default; it
// moved here when shellcore became embedder-agnostic.
//
// Hierarchy:
//
//	/log/    → PathQuery   (streamed via the coordinator)
//	/bin/    → PathLocal   (saved functions)
//	/cron.d/ → PathLocal   (watch definitions)
//	/etc/    → PathLocal   (config)
//	/        → PathLocal   (root listing)
//	other    → PathUnknown
//
// Wired onto a ShellCore via core.SetPathClassifier(shell.PathClassifier)
// at construction; passed to shellcore.RewritePaths as
// WithPathClassifier(shell.PathClassifier) when invoking the
// rewriter directly (non-interactive entry point).
func PathClassifier(path string) shellcore.PathKind {
	switch {
	case strings.HasPrefix(path, "/log/") || path == "/log":
		return shellcore.PathQuery
	case strings.HasPrefix(path, "/bin/") || path == "/bin",
		strings.HasPrefix(path, "/cron.d/") || path == "/cron.d",
		strings.HasPrefix(path, "/etc/") || path == "/etc",
		path == "/":
		return shellcore.PathLocal
	}
	return shellcore.PathUnknown
}
