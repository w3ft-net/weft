package shell

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type tokenKind int

const (
	tokSpace  tokenKind = iota
	tokPipe             // |
	tokCmd              // command name (first word or first after |)
	tokFlag             // -x, --long
	tokString           // 'single' or "double" quoted
	tokFile             // *.log, paths with / or globs
	tokText             // everything else
)

type token struct {
	kind  tokenKind
	value string
}

// Syntax-highlight styles using warp/weft logo palette.
var (
	pipeStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true).Inline(true)
	cmdStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Bold(true).Inline(true)
	flagStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("105")).Inline(true)
	stringStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Inline(true)
	fileStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("215")).Inline(true)
	textStyle   = lipgloss.NewStyle().Inline(true)
	cursorStyle = lipgloss.NewStyle().Reverse(true).Inline(true)
)

func styleFor(k tokenKind) lipgloss.Style {
	switch k {
	case tokPipe:
		return pipeStyle
	case tokCmd:
		return cmdStyle
	case tokFlag:
		return flagStyle
	case tokString:
		return stringStyle
	case tokFile:
		return fileStyle
	default:
		return textStyle
	}
}

// tokenize breaks a pipeline string into syntax tokens.
func tokenize(input string) []token {
	var tokens []token
	i := 0
	expectCmd := true

	for i < len(input) {
		// Whitespace.
		if input[i] == ' ' || input[i] == '\t' {
			j := i
			for j < len(input) && (input[j] == ' ' || input[j] == '\t') {
				j++
			}
			tokens = append(tokens, token{tokSpace, input[i:j]})
			i = j
			continue
		}

		// Pipe.
		if input[i] == '|' {
			tokens = append(tokens, token{tokPipe, "|"})
			i++
			expectCmd = true
			continue
		}

		// Single-quoted string.
		if input[i] == '\'' {
			j := i + 1
			for j < len(input) && input[j] != '\'' {
				j++
			}
			if j < len(input) {
				j++ // include closing quote
			}
			tokens = append(tokens, token{tokString, input[i:j]})
			i = j
			expectCmd = false
			continue
		}

		// Double-quoted string.
		if input[i] == '"' {
			j := i + 1
			for j < len(input) && input[j] != '"' {
				if input[j] == '\\' && j+1 < len(input) {
					j++ // skip escaped char
				}
				j++
			}
			if j < len(input) {
				j++ // include closing quote
			}
			tokens = append(tokens, token{tokString, input[i:j]})
			i = j
			expectCmd = false
			continue
		}

		// Word — read until delimiter.
		j := i
		for j < len(input) && input[j] != ' ' && input[j] != '\t' &&
			input[j] != '|' && input[j] != '\'' && input[j] != '"' {
			j++
		}
		word := input[i:j]

		if expectCmd {
			tokens = append(tokens, token{tokCmd, word})
			expectCmd = false
		} else if len(word) > 0 && word[0] == '-' {
			tokens = append(tokens, token{tokFlag, word})
		} else if isFileLike(word) {
			tokens = append(tokens, token{tokFile, word})
		} else {
			tokens = append(tokens, token{tokText, word})
		}
		i = j
	}

	return tokens
}

func isFileLike(word string) bool {
	return strings.HasSuffix(word, ".log") ||
		strings.Contains(word, "*.") ||
		strings.Contains(word, "/") ||
		word == "*"
}

// highlightPipeline returns a syntax-highlighted rendering of a pipeline string.
func highlightPipeline(input string) string {
	tokens := tokenize(input)
	var b strings.Builder
	for _, tok := range tokens {
		if tok.kind == tokSpace {
			b.WriteString(tok.value)
		} else {
			b.WriteString(styleFor(tok.kind).Render(tok.value))
		}
	}
	return b.String()
}

// highlightWithCursor renders a syntax-highlighted pipeline with a block
// cursor at the given character position. Used in place of textinput.View().
func highlightWithCursor(input string, cursorPos int) string {
	tokens := tokenize(input)
	var b strings.Builder
	charPos := 0

	for _, tok := range tokens {
		tokLen := len(tok.value)

		if cursorPos >= charPos && cursorPos < charPos+tokLen {
			// Cursor falls within this token — split around it.
			offset := cursorPos - charPos
			style := styleFor(tok.kind)

			if offset > 0 {
				b.WriteString(style.Render(tok.value[:offset]))
			}
			b.WriteString(cursorStyle.Render(string(tok.value[offset])))
			if offset+1 < tokLen {
				b.WriteString(style.Render(tok.value[offset+1:]))
			}
		} else if tok.kind == tokSpace {
			b.WriteString(tok.value)
		} else {
			b.WriteString(styleFor(tok.kind).Render(tok.value))
		}
		charPos += tokLen
	}

	// Cursor at end of input — render a visible block.
	if cursorPos >= charPos {
		b.WriteString(cursorStyle.Render(" "))
	}

	return b.String()
}
