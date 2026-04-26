package shellcore

import (
	"fmt"
	"strings"
)

// ANSI SGR escape sequences for syntax highlighting.
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

// PromptInfo holds the fields needed to render the prompt prefix.
type PromptInfo struct {
	RangeLabel string // e.g. "24h", "2h30m"
	User       string // e.g. "alice"
	Customer   string // e.g. "acme" — falls back to "weft" when empty
	Cwd        string // e.g. "/" or "/log"
}

// FormatPromptPrefix builds the visible prompt prefix: [24h] alice@acme ~ $
func FormatPromptPrefix(info PromptInfo) string {
	var b strings.Builder

	// Range bracket.
	if info.RangeLabel != "" {
		b.WriteString("[")
		b.WriteString(info.RangeLabel)
		b.WriteString("] ")
	}

	// user@<customer>; customer defaults to "weft" so the prompt still
	// reads sensibly when the shell is running pre-login (dev fixtures,
	// or before the JWT has been parsed by the CLI).
	if info.User != "" {
		b.WriteString(info.User)
		b.WriteString("@")
		if info.Customer != "" {
			b.WriteString(info.Customer)
		} else {
			b.WriteString("weft")
		}
		b.WriteString(" ")
	}

	// cwd: ~ for root, otherwise the path.
	cwd := info.Cwd
	if cwd == "" || cwd == "/" {
		cwd = "~"
	}
	b.WriteString(cwd)
	b.WriteString(" $ ")

	return b.String()
}

// RenderPrompt renders the input prompt line with ANSI escape sequences.
func RenderPrompt(mode Mode, searchBuf string, input string, cursorPos int, info PromptInfo) string {
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
		prefix := FormatPromptPrefix(info)
		b.WriteString(ansiPrompt)
		b.WriteString(prefix)
		b.WriteString(ansiReset)
		b.WriteString(HighlightWithCursor(input, cursorPos))
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

// (Dropdown completion menu removed — tab completion is now inline.)

// stripANSI removes all ANSI escape sequences from a string, returning only visible characters.
func stripANSI(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == ';') {
				j++
			}
			if j < len(s) {
				j++ // skip final byte (letter)
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// PromptPrefixWidth returns the number of visible characters in the prompt
// prefix (before user input) for a given mode.
func PromptPrefixWidth(mode Mode, searchBuf string, info PromptInfo) int {
	switch mode {
	case ModeSearching:
		// "(reverse-i-search)`<searchBuf>': "
		return len(fmt.Sprintf("(reverse-i-search)`%s': ", searchBuf))
	case ModeInput:
		return len(FormatPromptPrefix(info))
	default:
		return 0
	}
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
