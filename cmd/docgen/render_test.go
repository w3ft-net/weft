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
		body := renderShell(d.body, d.Shell.Banner, nil)
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
	out := renderShell(body, "", nil)
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
		out := renderShell(d.body, d.Shell.Banner, nil)
		first := strings.SplitN(out, "\n", 2)[0]
		if !strings.Contains(first, "Weft Commands") {
			t.Errorf("%s: first line lacks banner: %q", d.path, first)
		}
	}
}

// TestManualTablesRenderAsHTMLTables guards the web renderer against
// losing the GFM table extension: without it pipe tables come out as
// literal "| a | b |" paragraphs in the browser.
func TestManualTablesRenderAsHTMLTables(t *testing.T) {
	body := []byte("## Things\n\n| Name | Meaning |\n|------|---------|\n| a | first |\n| b | second |\n")
	html, _, err := renderTopic(body, "unrelated", nil)
	if err != nil {
		t.Fatalf("renderTopic: %v", err)
	}
	for _, want := range []string{"<table>", "<th>Name</th>", "<td>first</td>"} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered HTML missing %q:\n%s", want, html)
		}
	}
}

// TestShellRendersLinksAndTables covers the constructs the web-only
// manual topics bring into the shell renderer: inline links (which once
// recursed forever) and GFM tables (which rendered as raw pipes).
func TestShellRendersLinksAndTables(t *testing.T) {
	body := []byte("See [the docs](https://example.com/x) now.\n\n| Name | Meaning |\n|------|---------|\n| a | first |\n| bb | second |\n")
	out := renderShell(body, "", nil)
	for _, want := range []string{"the docs (https://example.com/x)", "Name", "a     first", "bb    second"} {
		if !strings.Contains(out, want) {
			t.Errorf("shell output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "|") {
		t.Errorf("shell output still has raw pipes:\n%s", out)
	}
}

// TestInternalLinksResolve covers relative-file links between docs: the
// web gets /manual/<section>#frag, the shell gets "label (man topic)",
// a link to a shell-only doc is unlinked on the web, and a bad target or
// heading is recorded as an error.
func TestInternalLinksResolve(t *testing.T) {
	src := &doc{
		path: "docs/manual/a.md",
		body: []byte("See [Builtins](../shell/b.md#files) and [alert](../shell/c.md) and [bad](nope.md).\n"),
	}
	b := &doc{
		path: "docs/shell/b.md", Web: &webConfig{Section: "builtins"},
		Shell: &shellConfig{Topics: []string{"help builtins"}},
		body:  []byte("## Files\n\ntext\n"),
	}
	c := &doc{path: "docs/shell/c.md", Shell: &shellConfig{Topics: []string{"man alert"}}, body: []byte("## X\n")}
	idx := newLinkIndex([]*doc{src, b, c})

	html, _, err := renderTopic(src.body, "t", linkCtxFor(idx, src))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, `href="/manual/builtins#files"`) {
		t.Errorf("web link not rewritten:\n%s", html)
	}
	if strings.Contains(html, "c.md") || !strings.Contains(html, "alert") {
		t.Errorf("shell-only link should drop to its label:\n%s", html)
	}
	out := renderShell(src.body, "", linkCtxFor(idx, src))
	if !strings.Contains(out, "Builtins (help builtins)") || !strings.Contains(out, "alert (man alert)") {
		t.Errorf("shell links wrong:\n%s", out)
	}
	if len(idx.errs) == 0 || !strings.Contains(idx.errs[0], "nope.md") {
		t.Errorf("want error for nope.md, got %v", idx.errs)
	}
}
