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
			return nil
		},
		OnQuit: func(force bool) {
			c.vim = nil
			c.mode = ModeInput
			// Clear screen and redraw shell.
			c.write([]byte("\x1b[2J\x1b[H"))
			c.scrollback.Reset()
			c.resetFlushState()
			c.Redraw()
		},
	}

	c.vim = &vimState{editor: editor, path: path}
	c.mode = ModeVim
	c.renderVim()
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
