// docgen generates shellcore/docs_gen.go (plain-text + ANSI consts used
// by `help` / `man` in the shell) and frontend/src/generated/docs/*.ts
// (HTML consumed by the web manual) from a tree of markdown files.
//
// Source of truth: docs/shell/*.md and docs/manual/*.md. Each file has
// YAML frontmatter declaring how the document should appear in the shell
// and/or on the web. Run via `go generate ./shellcore/...`.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var (
	docsDirs = flag.String("docs", "docs/shell,docs/manual", "comma-separated markdown input dirs")
	shellOut = flag.String("shell-out", "shell/help_corpus_gen.go", "generated Go file for shell consts")
	webOut   = flag.String("web-out", "frontend/src/generated/docs", "output dir for web TS modules")
)

func main() {
	flag.Parse()

	var all []*doc
	for _, dir := range splitCSV(*docsDirs) {
		docs, err := loadDir(dir)
		if err != nil {
			log.Fatalf("load %s: %v", dir, err)
		}
		all = append(all, docs...)
	}

	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	// Sanity-check: every doc needs to land *somewhere*. A file with
	// neither shell nor web frontmatter is almost certainly a mistake.
	for _, d := range all {
		if d.Shell == nil && d.Web == nil {
			log.Fatalf("%s: doc %q has neither shell: nor web: frontmatter", d.path, d.ID)
		}
	}

	all = withManualShellPages(all)

	if err := writeShell(*shellOut, all); err != nil {
		log.Fatalf("write shell: %v", err)
	}
	if err := writeWeb(*webOut, all); err != nil {
		log.Fatalf("write web: %v", err)
	}

	fmt.Printf("docgen: %d topics → %s, %s\n", len(all), *shellOut, *webOut)
}

func loadDir(dir string) ([]*doc, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*doc
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		p := filepath.Join(dir, e.Name())
		d, err := loadDoc(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, d)
	}
	return out, nil
}

func splitCSV(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// withManualShellPages gives every web-only manual topic a shell page
// reachable as `man <section>`, and adds a `man` index listing them.
// Hand-written shell pages (docs/shell) keep their own topics; a
// manual topic whose `man <section>` collides with one is skipped.
func withManualShellPages(all []*doc) []*doc {
	taken := map[string]bool{"man": true}
	for _, d := range all {
		if d.Shell != nil {
			for _, t := range d.Shell.Topics {
				taken[t] = true
			}
		}
	}
	var index strings.Builder
	index.WriteString("## Manual topics\n\nType `man <topic>`. The same pages are in the web manual.\n\n")
	byTopic := append([]*doc(nil), all...)
	sort.Slice(byTopic, func(i, j int) bool { return webSection(byTopic[i]) < webSection(byTopic[j]) })
	for _, d := range byTopic {
		if d.Shell != nil || d.Web == nil {
			continue
		}
		topic := "man " + d.Web.Section
		if taken[topic] {
			continue
		}
		d.Shell = &shellConfig{
			Const:  "Manual" + camel(d.Web.Section),
			Topics: []string{topic},
			Banner: strings.ToUpper(d.Web.Section) + "(1)",
		}
		fmt.Fprintf(&index, "- `%s` — %s\n", topic, d.Title)
	}
	index.WriteString("\nSee also `help` for shell commands and `man alert`, `man range` and friends.\n")
	return append(all, &doc{
		ID:    "man-index",
		Title: "man",
		Shell: &shellConfig{Const: "ManIndex", Topics: []string{"man"}, Banner: "MAN(1)"},
		body:  []byte(index.String()),
	})
}

// camel turns a kebab-case slug into CamelCase for a Go identifier.
func camel(slug string) string {
	var b strings.Builder
	for _, part := range strings.Split(slug, "-") {
		if part != "" {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String()
}

func webSection(d *doc) string {
	if d.Web == nil {
		return ""
	}
	return d.Web.Section
}
