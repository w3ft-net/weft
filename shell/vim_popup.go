package shell

import (
	"context"
	"errors"
	"fmt"
	"github.com/w3ft-net/weft/warning"
	"strings"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
	"github.com/graybeardcorp/warp/pkg/vim"
)

// MessageAmender is implemented by FS adapters that track a per-file
// commit-message column. The post-save popup calls
// AmendVersionMessage(path, message) to record the message the user
// typed in the follow-up vim buffer. FSes without it (the dev shell,
// the WASM shell's read-only browser) silently no-op — the popup
// still opens, the message just goes nowhere.
//
// Pre-A.4.5 this interface lived in warp's shellcore. Moved here
// because the popup itself is a weft concern; warp's vim layer no
// longer knows about it.
type MessageAmender interface {
	AmendVersionMessage(path, message string) error
}

// installVimPopupHooks wires the post-save commit-message popup onto
// core. It returns the VimHooks{OnSave, OnQuit} that should be merged
// with any other embedder hooks before SetVimHooks.
//
// Behavior contract (unchanged from the warp-resident version):
//   - :wq on a versioned path (see isVersionedPath) swaps the editor for
//     a follow-up commit-message buffer.
//   - :wq in the popup records the (comment-stripped) message via
//     MessageAmender if the FS implements it; empty messages no-op.
//   - :q! in the popup skips the recording.
//   - :wq on any other path returns to the prompt as usual.
//
// WriteFile's error may be a non-blocking warning.Warning (or an
// errors.Join of several) — the save already happened, but there's
// something to tell the user (e.g. the /cron.d truncation lint).
// store.Split separates that from an actual rejection: only a real
// blocking error becomes vim's StatusMsg and skips the popup: a
// Warning-only result still opens the popup as normal, with the
// warning text folded into its header (see openMessagePopup) since
// that's the one place guaranteed to render after a successful :wq —
// plain :w has no equivalent surface today (see cron-truncation-lint
// plan doc's "known limitation").
func installVimPopupHooks(core *shellcore.Shell) shellcore.VimHooks {
	// queuedPath/queuedWarnings, when queuedPath is non-empty, signal
	// that the next OnQuit on the originating editor should open the
	// popup (with these warnings folded in) instead of exiting. Lives
	// as closure variables shared between OnSave and OnQuit — no
	// shellcore state needed.
	var queuedPath string
	var queuedWarnings []string

	return shellcore.VimHooks{
		OnSave: func(path, content string) error {
			fs := core.GetFS()
			var writeErr error
			if fs != nil {
				writeErr = fs.WriteFile(context.Background(), path, []byte(content))
			}
			blocking, warnings := warning.Split(writeErr)
			if len(blocking) > 0 {
				return errors.Join(blocking...)
			}
			if isVersionedPath(path) {
				queuedPath = path
				queuedWarnings = warnings
			}
			return nil
		},
		OnQuit: func(force bool) error {
			if queuedPath == "" {
				// No popup queued — let shellcore's default OnQuit
				// run, which will exitVim.
				return nil
			}
			path := queuedPath
			warnings := queuedWarnings
			queuedPath = ""
			queuedWarnings = nil
			openMessagePopup(core, path, warnings)
			return nil
		},
	}
}

// isVersionedPath reports whether a save on path triggers the
// post-save commit-message popup: the versioned files, which are
// /bin/<name>, /cron.d/<name>, /etc/dashboards/<name> and /etc/notify.yaml
// — entries with a name component, not the bare directories.
func isVersionedPath(path string) bool {
	switch {
	case strings.HasPrefix(path, "/bin/") && len(path) > len("/bin/"):
		return true
	case strings.HasPrefix(path, "/cron.d/") && len(path) > len("/cron.d/"):
		return true
	case strings.HasPrefix(path, "/etc/dashboards/") && len(path) > len("/etc/dashboards/"):
		return true
	case path == "/etc/notify.yaml":
		return true
	}
	return false
}

// messagePopupHeader is the comment-prefixed buffer that opens after
// a save on a versioned path. The first non-comment, non-blank
// line(s) become the commit message; comments and blank lines are
// stripped.
const messagePopupHeader = `# Commit message for %s.
# One or more lines; the first line is the subject shown in ` + "`git log`." + `
# Save & exit (:wq) to record. Quit without saving (:q!) to skip.`

// openMessagePopup swaps the active vim editor for a follow-up
// commit-message buffer scoped to `path`. Save records the message
// via MessageAmender (when available); quit-force skips. warnings, if
// any, are folded in as `# WARNING: ...` comment lines ahead of the
// blank line the user types into — generic rendering, the same for
// any check that produced them.
func openMessagePopup(core *shellcore.Shell, path string, warnings []string) {
	var b strings.Builder
	fmt.Fprintf(&b, messagePopupHeader, path)
	b.WriteByte('\n')
	for _, w := range warnings {
		fmt.Fprintf(&b, "# WARNING: %s\n", w)
	}
	b.WriteString("\n")
	header := b.String()
	hooks := vim.EditorHooks{
		OnSave: func(text string) error {
			msg := stripCommentLines(text)
			if msg != "" {
				if amender, ok := core.GetFS().(MessageAmender); ok {
					_ = amender.AmendVersionMessage(path, msg)
				}
			}
			return nil
		},
		OnQuit: func(force bool) {
			core.ExitVim()
		},
	}
	core.StartVimBuffer(":git-message:", header, hooks)
	// Move the cursor to the empty line below the comments so the
	// user can start typing immediately.
	if ed := core.VimEditor(); ed != nil {
		for ed.Cursor.Row+1 < ed.Buffer.LineCount() {
			ed.Cursor.Row++
		}
	}
}

// stripCommentLines drops `#`-prefixed and blank lines from a
// commit-message buffer, and trims leading/trailing whitespace from
// what remains. An entirely-stripped buffer (just comments + blanks)
// returns "" — equivalent to skipping.
func stripCommentLines(s string) string {
	lines := strings.Split(s, "\n")
	keep := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}
