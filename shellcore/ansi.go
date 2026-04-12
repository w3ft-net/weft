package shellcore

import (
	"fmt"
	"strings"
)

// ANSI SGR escape sequences for syntax highlighting.
// These mirror the lipgloss styles in shell/highlight.go and shell/complete.go.
const (
	ansiReset = "\033[0m"
	ansiBold  = "\033[1m"

	// Token colors (256-color mode).
	ansiPipe    = "\033[1;38;5;208m" // bold orange
	ansiCmd     = "\033[1;38;5;15m"  // bold white
	ansiFlag    = "\033[38;5;105m"   // purple
	ansiString  = "\033[38;5;2m"     // green
	ansiFile    = "\033[38;5;215m"   // light orange
	ansiText    = ""                 // default
	ansiCursor  = "\033[7m"          // reverse video
	ansiPrompt  = "\033[1;38;5;6m"   // bold cyan
	ansiError   = "\033[38;5;1m"     // red
	ansiDim     = "\033[38;5;8m"     // gray
	ansiSearch  = "\033[38;5;3m"     // yellow
	ansiAccent  = "\033[38;5;208m"   // orange
	ansiSpinner = "\033[38;5;208m"   // orange
	ansiMatch   = "\033[1;38;5;6m"   // bold cyan
	ansiNoMatch = "\033[38;5;7m"     // light gray
	ansiSelected = "\033[1;48;5;8;38;5;15m" // bold white on gray bg
)

// ansiForToken returns the ANSI SGR prefix for a token kind.
func ansiForToken(k TokenKind) string {
	switch k {
	case TokPipe:
		return ansiPipe
	case TokCmd:
		return ansiCmd
	case TokFlag:
		return ansiFlag
	case TokString:
		return ansiString
	case TokFile:
		return ansiFile
	default:
		return ansiText
	}
}

// HighlightPipeline returns a pipeline string with ANSI syntax highlighting.
func HighlightPipeline(input string) string {
	tokens := Tokenize(input)
	var b strings.Builder
	for _, tok := range tokens {
		if tok.Kind == TokSpace {
			b.WriteString(tok.Value)
		} else {
			sgr := ansiForToken(tok.Kind)
			if sgr != "" {
				b.WriteString(sgr)
				b.WriteString(tok.Value)
				b.WriteString(ansiReset)
			} else {
				b.WriteString(tok.Value)
			}
		}
	}
	return b.String()
}

// HighlightWithCursor renders a syntax-highlighted pipeline with a block
// cursor at the given character position using ANSI escape sequences.
func HighlightWithCursor(input string, cursorPos int) string {
	tokens := Tokenize(input)
	var b strings.Builder
	charPos := 0

	for _, tok := range tokens {
		tokLen := len(tok.Value)

		if cursorPos >= charPos && cursorPos < charPos+tokLen {
			offset := cursorPos - charPos
			sgr := ansiForToken(tok.Kind)

			if offset > 0 {
				if sgr != "" {
					b.WriteString(sgr)
				}
				b.WriteString(tok.Value[:offset])
				if sgr != "" {
					b.WriteString(ansiReset)
				}
			}
			b.WriteString(ansiCursor)
			b.WriteByte(tok.Value[offset])
			b.WriteString(ansiReset)
			if offset+1 < tokLen {
				if sgr != "" {
					b.WriteString(sgr)
				}
				b.WriteString(tok.Value[offset+1:])
				if sgr != "" {
					b.WriteString(ansiReset)
				}
			}
		} else if tok.Kind == TokSpace {
			b.WriteString(tok.Value)
		} else {
			sgr := ansiForToken(tok.Kind)
			if sgr != "" {
				b.WriteString(sgr)
				b.WriteString(tok.Value)
				b.WriteString(ansiReset)
			} else {
				b.WriteString(tok.Value)
			}
		}
		charPos += tokLen
	}

	// Cursor at end of input — render a visible block.
	if cursorPos >= charPos {
		b.WriteString(ansiCursor)
		b.WriteByte(' ')
		b.WriteString(ansiReset)
	}

	return b.String()
}

// SpinnerFrames are the braille dot cycle used for the query spinner.
var SpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// RenderSpinner returns a single spinner frame with ANSI color.
func RenderSpinner(frame int) string {
	f := SpinnerFrames[frame%len(SpinnerFrames)]
	return ansiSpinner + f + ansiReset
}

// RenderPrompt renders the input prompt line with ANSI escape sequences.
func RenderPrompt(mode Mode, searchBuf string, input string, cursorPos int) string {
	var b strings.Builder

	switch mode {
	case ModeRunning:
		// Handled by caller (spinner + message).
	case ModeSearching:
		b.WriteString(ansiSearch)
		fmt.Fprintf(&b, "(reverse-i-search)`%s': ", searchBuf)
		b.WriteString(ansiReset)
		b.WriteString(HighlightPipeline(input))
	default:
		b.WriteString(ansiPrompt)
		b.WriteString("$ ")
		b.WriteString(ansiReset)
		b.WriteString(HighlightPipeline(input))
	}

	return b.String()
}

// RenderRunningPrompt renders the spinner + "querying shards..." line.
func RenderRunningPrompt(spinnerFrame int) string {
	var b strings.Builder
	b.WriteString(RenderSpinner(spinnerFrame))
	b.WriteString(ansiDim)
	b.WriteString(" querying shards...")
	b.WriteString(ansiReset)
	return b.String()
}

// RenderCompleterMenu returns the completion menu as ANSI text.
func RenderCompleterMenu(c *Completer) string {
	if !c.Active || len(c.Items) == 0 {
		return ""
	}

	maxVisible := 8
	start := 0
	if len(c.Items) > maxVisible {
		start = c.Cursor - maxVisible/2
		if start < 0 {
			start = 0
		}
		if start+maxVisible > len(c.Items) {
			start = len(c.Items) - maxVisible
		}
	}
	end := start + maxVisible
	if end > len(c.Items) {
		end = len(c.Items)
	}

	var sb strings.Builder
	prefixLen := len(c.Prefix)

	for i := start; i < end; i++ {
		item := c.Items[i]

		if i == c.Cursor {
			sb.WriteString("  ")
			sb.WriteString(ansiSelected)
			sb.WriteString(item.Text)
			sb.WriteString(ansiReset)
		} else {
			sb.WriteString("  ")
			if prefixLen <= len(item.Text) {
				sb.WriteString(ansiMatch)
				sb.WriteString(item.Text[:prefixLen])
				sb.WriteString(ansiReset)
				sb.WriteString(ansiNoMatch)
				sb.WriteString(item.Text[prefixLen:])
				sb.WriteString(ansiReset)
			} else {
				sb.WriteString(ansiNoMatch)
				sb.WriteString(item.Text)
				sb.WriteString(ansiReset)
			}
		}

		if item.Kind == KindFile {
			sb.WriteString(ansiDim)
			sb.WriteString(" (file)")
			sb.WriteString(ansiReset)
		}

		sb.WriteByte('\n')
	}

	if len(c.Items) > maxVisible {
		sb.WriteString(ansiDim)
		fmt.Fprintf(&sb, "  ... %d more", len(c.Items)-maxVisible)
		sb.WriteString(ansiReset)
		sb.WriteByte('\n')
	}

	return sb.String()
}

// ANSI helpers for output formatting.

// ErrorText wraps text in error (red) ANSI styling.
func ErrorText(s string) string {
	return ansiError + s + ansiReset
}

// DimText wraps text in dim (gray) ANSI styling.
func DimText(s string) string {
	return ansiDim + s + ansiReset
}

// AccentText wraps text in accent (orange) ANSI styling.
func AccentText(s string) string {
	return ansiAccent + s + ansiReset
}

// PromptText wraps text in prompt (bold cyan) ANSI styling.
func PromptText(s string) string {
	return ansiPrompt + s + ansiReset
}
