package shellcore

import "strings"

// captureState holds state for the stdin capture mode (cat > /path).
type captureState struct {
	path    string
	lines   []string
	current []rune // current line being typed
}

// startCapture enters stdin capture mode for writing to a file.
func (c *ShellCore) startCapture(path string) {
	c.capture = &captureState{
		path:  path,
		lines: nil,
	}
	c.mode = ModeCapture
	c.renderCapturePrompt()
}

// handleCaptureKey processes keystrokes in capture mode.
func (c *ShellCore) handleCaptureKey(ev KeyEvent) {
	cap := c.capture
	if cap == nil {
		return
	}

	switch ev.Key {
	case KeyCtrlD:
		// Finish capture — write to FS.
		content := strings.Join(cap.lines, "\n")
		if len(cap.current) > 0 {
			if content != "" {
				content += "\n"
			}
			content += string(cap.current)
		}
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}

		if c.shellFS != nil {
			if err := c.shellFS.WriteFile(cap.path, []byte(content)); err != nil {
				c.appendScrollback(ErrorText(err.Error()) + "\n")
			}
		}

		c.capture = nil
		c.mode = ModeInput
		c.redrawPrompt()
		return

	case KeyCtrlC:
		// Cancel capture.
		c.appendScrollback(DimText("^C") + "\n")
		c.capture = nil
		c.mode = ModeInput
		c.redrawPrompt()
		return

	case KeyEnter:
		// Commit current line.
		line := string(cap.current)
		cap.lines = append(cap.lines, line)
		cap.current = nil
		c.write([]byte("\r\n"))
		c.renderCapturePrompt()
		return

	case KeyBackspace:
		if len(cap.current) > 0 {
			cap.current = cap.current[:len(cap.current)-1]
			c.renderCaptureLine()
		}
		return

	case KeyNone:
		if ev.Rune != 0 {
			cap.current = append(cap.current, ev.Rune)
			c.renderCaptureLine()
			return
		}
	}
}

// renderCapturePrompt writes the continuation prompt.
func (c *ShellCore) renderCapturePrompt() {
	c.write([]byte(DimText("> ")))
}

// renderCaptureLine redraws the current capture line.
func (c *ShellCore) renderCaptureLine() {
	cap := c.capture
	if cap == nil {
		return
	}
	c.write([]byte("\r" + DimText("> ") + string(cap.current) + "\x1b[K"))
}
