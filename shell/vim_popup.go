package shell

import (
	"fmt"
	"strings"

	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/warp/pkg/vim"
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
//   - :wq on /bin/<name> or /cron.d/<name> swaps the editor for a
//     follow-up commit-message buffer.
//   - :wq in the popup records the (comment-stripped) message via
//     MessageAmender if the FS implements it; empty messages no-op.
//   - :q! in the popup skips the recording.
//   - :wq on any other path returns to the prompt as usual.
func installVimPopupHooks(core *shellcore.ShellCore) shellcore.VimHooks {
	// queuedPath, when non-empty, signals that the next OnQuit on
	// the originating editor should open the popup instead of
	// exiting. Lives as a closure variable shared between OnSave
	// and OnQuit — no shellcore state needed.
	var queuedPath string

	return shellcore.VimHooks{
		OnSave: func(path, content string) error {
			fs := core.GetFS()
			if fs != nil {
				if err := fs.WriteFile(path, []byte(content)); err != nil {
					return err
				}
			}
			core.Dispatch(shellcore.ActionVimSave{Path: path, Content: content})
			if isVersionedPath(path) {
				queuedPath = path
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
			queuedPath = ""
			openMessagePopup(core, path)
			return nil
		},
	}
}

// isVersionedPath reports whether a save on path triggers the
// post-save commit-message popup. /bin/<name> and /cron.d/<name>
// — entries with a name component, not the bare directories.
func isVersionedPath(path string) bool {
	switch {
	case strings.HasPrefix(path, "/bin/") && len(path) > len("/bin/"):
		return true
	case strings.HasPrefix(path, "/cron.d/") && len(path) > len("/cron.d/"):
		return true
	}
	return false
}

// messagePopupHeader is the comment-prefixed buffer that opens after
// a save on a versioned path. The first non-comment, non-blank
// line(s) become the commit message; comments and blank lines are
// stripped.
const messagePopupHeader = `# Commit message for %s.
# One or more lines; the first line is the subject shown in ` + "`wit log`." + `
# Save & exit (:wq) to record. Quit without saving (:q!) to skip.

`

// openMessagePopup swaps the active vim editor for a follow-up
// commit-message buffer scoped to `path`. Save records the message
// via MessageAmender (when available); quit-force skips.
func openMessagePopup(core *shellcore.ShellCore, path string) {
	header := fmt.Sprintf(messagePopupHeader, path)
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
	core.StartVimBuffer(":wit-message:", header, hooks)
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
