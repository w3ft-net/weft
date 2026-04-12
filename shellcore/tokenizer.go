package shellcore

import "strings"

// TokenKind classifies a syntax token.
type TokenKind int

const (
	TokSpace  TokenKind = iota
	TokPipe             // |
	TokCmd              // command name (first word or first after |)
	TokFlag             // -x, --long
	TokString           // 'single' or "double" quoted
	TokFile             // *.log, paths with / or globs
	TokText             // everything else
)

// Token is a single syntax-highlighted element of a pipeline.
type Token struct {
	Kind  TokenKind
	Value string
}

// Tokenize breaks a pipeline string into syntax tokens.
func Tokenize(input string) []Token {
	var tokens []Token
	i := 0
	expectCmd := true

	for i < len(input) {
		// Whitespace.
		if input[i] == ' ' || input[i] == '\t' {
			j := i
			for j < len(input) && (input[j] == ' ' || input[j] == '\t') {
				j++
			}
			tokens = append(tokens, Token{TokSpace, input[i:j]})
			i = j
			continue
		}

		// Pipe.
		if input[i] == '|' {
			tokens = append(tokens, Token{TokPipe, "|"})
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
			tokens = append(tokens, Token{TokString, input[i:j]})
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
			tokens = append(tokens, Token{TokString, input[i:j]})
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
			tokens = append(tokens, Token{TokCmd, word})
			expectCmd = false
		} else if len(word) > 0 && word[0] == '-' {
			tokens = append(tokens, Token{TokFlag, word})
		} else if IsFileLike(word) {
			tokens = append(tokens, Token{TokFile, word})
		} else {
			tokens = append(tokens, Token{TokText, word})
		}
		i = j
	}

	return tokens
}

// IsFileLike returns true if the word looks like a filename or glob.
func IsFileLike(word string) bool {
	return strings.HasSuffix(word, ".log") ||
		strings.Contains(word, "*.") ||
		strings.Contains(word, "/") ||
		word == "*"
}
