package shellcore

import (
	"testing"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect []Token
	}{
		{
			name:  "simple command",
			input: "grep ERROR",
			expect: []Token{
				{TokCmd, "grep"},
				{TokSpace, " "},
				{TokText, "ERROR"},
			},
		},
		{
			name:  "pipeline",
			input: "cat api.log | grep ERROR | sort",
			expect: []Token{
				{TokCmd, "cat"},
				{TokSpace, " "},
				{TokFile, "api.log"},
				{TokSpace, " "},
				{TokPipe, "|"},
				{TokSpace, " "},
				{TokCmd, "grep"},
				{TokSpace, " "},
				{TokText, "ERROR"},
				{TokSpace, " "},
				{TokPipe, "|"},
				{TokSpace, " "},
				{TokCmd, "sort"},
			},
		},
		{
			name:  "flags",
			input: "sort -rn -k 3",
			expect: []Token{
				{TokCmd, "sort"},
				{TokSpace, " "},
				{TokFlag, "-rn"},
				{TokSpace, " "},
				{TokFlag, "-k"},
				{TokSpace, " "},
				{TokText, "3"},
			},
		},
		{
			name:  "single quoted string",
			input: "awk '{print $4}'",
			expect: []Token{
				{TokCmd, "awk"},
				{TokSpace, " "},
				{TokString, "'{print $4}'"},
			},
		},
		{
			name:  "double quoted string",
			input: `grep "hello world"`,
			expect: []Token{
				{TokCmd, "grep"},
				{TokSpace, " "},
				{TokString, `"hello world"`},
			},
		},
		{
			name:  "glob file",
			input: "cat *.log",
			expect: []Token{
				{TokCmd, "cat"},
				{TokSpace, " "},
				{TokFile, "*.log"},
			},
		},
		{
			name:  "path file",
			input: "cat /var/log/app.log",
			expect: []Token{
				{TokCmd, "cat"},
				{TokSpace, " "},
				{TokFile, "/var/log/app.log"},
			},
		},
		{
			name:  "pipe resets command expectation",
			input: "cat foo | awk '{x}' | sort -r",
			expect: []Token{
				{TokCmd, "cat"},
				{TokSpace, " "},
				{TokText, "foo"},
				{TokSpace, " "},
				{TokPipe, "|"},
				{TokSpace, " "},
				{TokCmd, "awk"},
				{TokSpace, " "},
				{TokString, "'{x}'"},
				{TokSpace, " "},
				{TokPipe, "|"},
				{TokSpace, " "},
				{TokCmd, "sort"},
				{TokSpace, " "},
				{TokFlag, "-r"},
			},
		},
		{
			name:   "empty input",
			input:  "",
			expect: nil,
		},
		{
			name:  "jq pipeline",
			input: `jq 'select(.status > 500)' | jq '{path, status}'`,
			expect: []Token{
				{TokCmd, "jq"},
				{TokSpace, " "},
				{TokString, "'select(.status > 500)'"},
				{TokSpace, " "},
				{TokPipe, "|"},
				{TokSpace, " "},
				{TokCmd, "jq"},
				{TokSpace, " "},
				{TokString, "'{path, status}'"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Tokenize(tt.input)
			if len(got) != len(tt.expect) {
				t.Fatalf("Tokenize(%q): got %d tokens, want %d\n  got:  %v\n  want: %v",
					tt.input, len(got), len(tt.expect), got, tt.expect)
			}
			for i := range got {
				if got[i].Kind != tt.expect[i].Kind || got[i].Value != tt.expect[i].Value {
					t.Errorf("token[%d]: got {%d %q}, want {%d %q}",
						i, got[i].Kind, got[i].Value, tt.expect[i].Kind, tt.expect[i].Value)
				}
			}
		})
	}
}
