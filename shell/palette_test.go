package shell

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/graybeardcorp/warp/pkg/syntax"
)

// TestWeftPalette_DerivesFromLogo asserts the theme actually paints the
// logo's brand threads onto the token kinds that carry weft's identity:
// commands in brand blue (warp thread 33), string literals in gold (weft
// thread 214), operators in brand orange (208). If someone retints the
// palette off-brand, these break.
func TestWeftPalette_DerivesFromLogo(t *testing.T) {
	out := syntax.RenderWithPalette(syntax.LangBash,
		`for f in *.log; do grep "error" $f | wc -l; done`, weftPalette())

	for _, want := range []struct {
		code, desc string
	}{
		{"38;5;33m", "command in logo brand blue"},
		{"38;5;214m", "string literal in logo gold"},
		{"38;5;208m", "operator/pipe in logo orange"},
		{"38;5;141m", "keyword in violet"},
	} {
		if !strings.Contains(out, want.code) {
			t.Errorf("rendered bash missing %s (%s)\ngot: %q", want.code, want.desc, out)
		}
	}
}

// TestWeftPalette_Showcase isn't an assertion — it renders representative
// snippets through the theme and prints them so a human running
// `go test ./shell/ -run Showcase -v` sees the real colors in their
// terminal.
func TestWeftPalette_Showcase(t *testing.T) {
	p := weftPalette()
	samples := []struct {
		title, lang, src string
	}{
		{"bash pipeline", string(syntax.LangBash),
			"grep -i \"timeout\" warp.log \\\n  | awk '{print $1, $5}' \\\n  | sort | uniq -c # count by host"},
		{"lua", string(syntax.LangLua),
			"local function tally(rows)\n  local n = 0\n  for _, r in ipairs(rows) do n = n + 1 end\n  return n -- total\nend"},
		{"go", string(syntax.LangGo),
			"func tally(rows []Row) int {\n\tn := 0\n\tfor _, r := range rows {\n\t\tif r.Raw != \"\" { // skip blanks\n\t\t\tn++\n\t\t}\n\t}\n\treturn n\n}"},
		{"python", string(syntax.LangPython),
			"def tally(rows):\n    n = 0\n    for r in rows:\n        if r.raw:  # skip blanks\n            n += 1\n    return n  # total"},
		{"diff", string(syntax.LangDiff),
			"--- a/shell/shell.go\n+++ b/shell/shell.go\n@@ -106,6 +106,8 @@\n-\tcore.SetSyntaxFallback(bash)\n+\tcore.SetPalette(weftPalette())"},
		{"markdown", string(syntax.LangMarkdown),
			"# weft shell\nQuery logs with *Unix idioms*. See `weftPalette()`.\n- pipe into [chart](https://w3ft.net)\n> derived from the logo"},
	}
	fmt.Fprint(os.Stdout, "\n\x1b[1mweft syntax palette\x1b[0m (derived from the startup logo)\n\n")
	for _, s := range samples {
		fmt.Fprintf(os.Stdout, "\x1b[2m── %s ─────────────\x1b[0m\n", s.title)
		fmt.Fprintln(os.Stdout, syntax.RenderWithPalette(syntax.Lang(s.lang), s.src, p))
		fmt.Fprintln(os.Stdout)
	}
}
