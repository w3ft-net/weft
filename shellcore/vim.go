package shellcore

import (
	"fmt"
	"strings"

	"github.com/kevlarcon/warp/pkg/vim"
)

// vimState holds the active vim editor session.
type vimState struct {
	editor *vim.Editor
	path   string
	// pendingMessagePath, when non-empty, signals that the next
	// OnQuit should not exit vim — instead, it should swap the
	// editor for a follow-up buffer where the user types a commit
	// message. Set by OnSave for /bin and /cron.d paths only.
	pendingMessagePath string
	// messageMode is true when the current editor IS the follow-up
	// commit-message buffer. In that mode OnSave records the message
	// (rather than writing a file) and OnQuit exits cleanly.
	messageMode bool
}

// startVim initiates a vim editing session.
// The renderer should respond by reading the file and sending a VimFileLoadedEvent.
func (c *ShellCore) startVim(path string) {
	path = normalizePath(path)

	// Read file content from the FS. If the file doesn't exist, start empty.
	content := ""
	if data, err := c.shellFS.ReadFile(path); err == nil {
		content = string(data)
	}

	editor := vim.NewEditor(content)
	editor.Hooks = vim.EditorHooks{
		OnSave: func(text string) error {
			if err := c.shellFS.WriteFile(path, []byte(text)); err != nil {
				return err
			}
			c.dispatch(ActionVimSave{Path: path, Content: text})
			// Versioned namespaces get a follow-up commit-message
			// buffer when the user exits. The flag is consumed in
			// OnQuit; if the user :w-then-edits-more, the next :w
			// will overwrite it, which is fine.
			if isVersionedPath(path) {
				c.vim.pendingMessagePath = path
			}
			return nil
		},
		OnQuit: func(force bool) {
			if c.vim != nil && c.vim.pendingMessagePath != "" && !c.vim.messageMode {
				c.openMessagePopup(c.vim.pendingMessagePath)
				return
			}
			c.exitVim()
		},
	}

	c.vim = &vimState{editor: editor, path: path}
	c.mode = ModeVim
	c.renderVim()
}

// exitVim restores the shell to its input mode. Called for normal
// vi exits and for the message-popup's exit.
func (c *ShellCore) exitVim() {
	c.vim = nil
	c.mode = ModeInput
	c.write([]byte("\x1b[2J\x1b[H"))
	c.scrollback.Reset()
	c.resetFlushState()
	c.Redraw()
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

// messagePopupTemplate is the comment-prefixed buffer that opens
// after a save on a versioned path. The first non-comment, non-blank
// line(s) become the commit message; comments and blank lines are
// stripped.
const messagePopupHeader = `# Commit message for %s.
# One or more lines; the first line is the subject shown in ` + "`wit log`." + `
# Save & exit (:wq) to record. Quit without saving (:q!) to skip.

`

// openMessagePopup swaps the active vim editor for a follow-up
// commit-message buffer scoped to `path`. Save records the message
// via MessageAmender (when available); quit-force skips.
func (c *ShellCore) openMessagePopup(path string) {
	header := fmt.Sprintf(messagePopupHeader, path)
	editor := vim.NewEditor(header)
	// Move the cursor to the empty line below the comments so the
	// user can start typing immediately.
	for editor.Cursor.Row+1 < editor.Buffer.LineCount() {
		editor.Cursor.Row++
	}
	editor.Hooks = vim.EditorHooks{
		OnSave: func(text string) error {
			msg := stripCommentLines(text)
			if msg != "" {
				if amender, ok := c.shellFS.(MessageAmender); ok {
					_ = amender.AmendVersionMessage(path, msg)
				}
			}
			return nil
		},
		OnQuit: func(force bool) {
			c.exitVim()
		},
	}
	c.vim = &vimState{
		editor:      editor,
		path:        ":wit-message:",
		messageMode: true,
	}
	c.mode = ModeVim
	c.renderVim()
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

// handleVimKey processes a keystroke in vim mode.
func (c *ShellCore) handleVimKey(ev KeyEvent) {
	if c.vim == nil {
		return
	}

	key := vimKeyString(ev)
	if key == "" {
		return
	}

	// Clear status message on any keypress.
	c.vim.editor.StatusMsg = ""

	c.vim.editor.ProcessKey(key)

	// If the hooks triggered OnQuit, mode is already ModeInput.
	if c.mode != ModeVim {
		return
	}

	c.renderVim()
}

// renderVim draws the vim editor state to the terminal via the write callback.
func (c *ShellCore) renderVim() {
	if c.vim == nil {
		return
	}

	e := c.vim.editor
	var b strings.Builder

	// Move cursor home and clear screen.
	b.WriteString("\x1b[H\x1b[2J")

	// Render buffer lines.
	height := c.height - 2 // leave 2 lines for status + command
	if height < 1 {
		height = 1
	}

	// Simple viewport: show lines around the cursor.
	startLine := 0
	if e.Cursor.Row >= height {
		startLine = e.Cursor.Row - height + 1
	}

	for i := 0; i < height; i++ {
		lineIdx := startLine + i
		if lineIdx < e.Buffer.LineCount() {
			line := e.Buffer.Line(lineIdx)
			// Truncate to terminal width.
			if len(line) > c.width {
				line = line[:c.width]
			}
			b.WriteString(line)
		} else {
			// Tilde for empty lines past end of file.
			b.WriteString(ansiDim)
			b.WriteByte('~')
			b.WriteString(ansiReset)
		}
		b.WriteString("\r\n")
	}

	// Status line.
	mode := e.Mode.String()
	filename := c.vim.path
	lineInfo := fmt.Sprintf("%d/%d", e.Cursor.Row+1, e.Buffer.LineCount())

	statusLeft := fmt.Sprintf(" -- %s -- %s", mode, filename)
	statusRight := lineInfo + " "
	pad := c.width - len(statusLeft) - len(statusRight)
	if pad < 0 {
		pad = 0
	}

	b.WriteString("\x1b[7m") // reverse video for status bar
	b.WriteString(statusLeft)
	b.WriteString(strings.Repeat(" ", pad))
	b.WriteString(statusRight)
	b.WriteString(ansiReset)
	b.WriteString("\r\n")

	// Command line / status message.
	if e.Mode == vim.ModeCommand {
		b.WriteByte(byte(e.CmdPrefix))
		b.WriteString(e.CmdBuf)
	} else if e.StatusMsg != "" {
		b.WriteString(ansiError)
		b.WriteString(e.StatusMsg)
		b.WriteString(ansiReset)
	}

	// Position cursor.
	screenRow := e.Cursor.Row - startLine + 1
	screenCol := e.Cursor.Col + 1
	fmt.Fprintf(&b, "\x1b[%d;%dH", screenRow, screenCol)

	c.write([]byte(b.String()))
}

// vimKeyString converts a shellcore KeyEvent to a vim key string.
func vimKeyString(ev KeyEvent) string {
	switch ev.Key {
	case KeyEnter:
		return "Enter"
	case KeyBackspace:
		return "Backspace"
	case KeyTab:
		return "\t"
	case KeyEsc:
		return "\x1b"
	case KeyUp:
		return "ArrowUp"
	case KeyDown:
		return "ArrowDown"
	case KeyLeft:
		return "ArrowLeft"
	case KeyRight:
		return "ArrowRight"
	case KeyDelete:
		return "Delete"
	case KeyHome:
		return "Home"
	case KeyEnd:
		return "End"
	case KeyCtrlA:
		return "\x01"
	case KeyCtrlC:
		return "\x03"
	case KeyCtrlD:
		return "\x04"
	case KeyCtrlE:
		return "\x05"
	case KeyCtrlK:
		return "\x0b"
	case KeyCtrlR:
		return "\x12"
	case KeyCtrlU:
		return "\x15"
	case KeyCtrlW:
		return "\x17"
	case KeyPgUp, KeyPgDown, KeyShiftTab:
		return "" // not used in vim
	case KeyNone:
		if ev.Rune != 0 {
			return string(ev.Rune)
		}
	}
	return ""
}
