package main

import (
	"os"
	"strings"
	"testing"
)

// TestAllShellDocsRender is a corpus-level sanity check: every
// markdown file in docs/shell/ parses, has a const name, and yields
// non-empty text. Failures dump the file path.
func TestAllShellDocsRender(t *testing.T) {
	docs, err := loadDir("../../docs/shell")
	if err != nil {
		t.Fatalf("loadDir: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("no docs found in docs/shell")
	}
	for _, d := range docs {
		if d.Shell == nil {
			t.Errorf("%s: no shell frontmatter", d.path)
			continue
		}
		if d.Shell.Const == "" {
			t.Errorf("%s: shell.const empty", d.path)
		}
		body := renderShell(d.body, d.Shell.Banner)
		if strings.TrimSpace(body) == "" {
			t.Errorf("%s: renders empty", d.path)
		}
	}
}

// TestHelpCronPreservesTierSentinel confirms the {{TIER_LIMITS}}
// placeholder survives through the renderer unmolested — the shell's
// helpCron() relies on splitting on it at runtime.
func TestHelpCronPreservesTierSentinel(t *testing.T) {
	raw, err := os.ReadFile("../../docs/shell/help-cron.md")
	if err != nil {
		t.Fatal(err)
	}
	_, body, err := splitFrontmatter(raw)
	if err != nil {
		t.Fatal(err)
	}
	out := renderShell(body, "")
	if !strings.Contains(out, "{{TIER_LIMITS}}") {
		t.Errorf("{{TIER_LIMITS}} sentinel was altered by renderer\n%s", out)
	}
}

// TestManPagesHaveBanner confirms each man-*.md doc produces a
// centered CMD(1) banner as its first line.
func TestManPagesHaveBanner(t *testing.T) {
	docs, err := loadDir("../../docs/shell")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if !strings.HasPrefix(d.ID, "man-") {
			continue
		}
		if d.Shell == nil || d.Shell.Banner == "" {
			t.Errorf("%s: man-* doc missing shell.banner", d.path)
			continue
		}
		out := renderShell(d.body, d.Shell.Banner)
		first := strings.SplitN(out, "\n", 2)[0]
		if !strings.Contains(first, "Weft Commands") {
			t.Errorf("%s: first line lacks banner: %q", d.path, first)
		}
	}
}
