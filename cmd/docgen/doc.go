package main

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// doc is one markdown source file after frontmatter extraction.
type doc struct {
	path  string
	ID    string       `yaml:"id"`
	Title string       `yaml:"title"`
	Shell *shellConfig `yaml:"shell"`
	Web   *webConfig   `yaml:"web"`

	body []byte // markdown body, frontmatter stripped
}

// shellConfig controls how a doc appears in the interactive shell.
type shellConfig struct {
	// Const is the exported Go identifier the generated file will
	// declare, e.g. "HelpAlerts". Must be a valid Go identifier.
	Const string `yaml:"const"`
	// Topics lists the dispatch strings a user would type to see this
	// content — e.g. ["help alerts"], or ["man alert", "alert --help"].
	Topics []string `yaml:"topics"`
	// Banner, if set, renders as the first line (`FOO(1) ... Weft
	// Commands ... FOO(1)`) before the body.
	Banner string `yaml:"banner"`
}

// webConfig controls how a doc appears in the /manual web page.
type webConfig struct {
	// Section is the URL slug (`/manual/<section>`) and the anchor id
	// used inside the rendered topic.
	Section string `yaml:"section"`
	// Category groups this doc in the manual sidebar and landing page.
	// Must match a key in cmd/docgen's known category map.
	Category string `yaml:"category"`
	// Order controls render order within the category. Lower first.
	Order int `yaml:"order"`
}

// loadDoc reads a markdown file with YAML frontmatter.
func loadDoc(path string) (*doc, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fm, body, err := splitFrontmatter(raw)
	if err != nil {
		return nil, err
	}
	var d doc
	if err := yaml.Unmarshal(fm, &d); err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	if d.ID == "" {
		return nil, fmt.Errorf("frontmatter: `id` is required")
	}
	if d.Title == "" {
		return nil, fmt.Errorf("frontmatter: `title` is required")
	}
	body, err = substituteVersion(body, weftdVersionFile)
	if err != nil {
		return nil, err
	}
	d.path = path
	d.body = body
	return &d, nil
}

// splitFrontmatter returns (frontmatter, body). Frontmatter is the block
// between the first two `---` lines. If the file doesn't start with
// `---`, frontmatter is empty and the whole file is the body.
func splitFrontmatter(raw []byte) ([]byte, []byte, error) {
	sep := []byte("---\n")
	if !bytes.HasPrefix(raw, sep) {
		return nil, raw, nil
	}
	after := raw[len(sep):]
	end := bytes.Index(after, []byte("\n---\n"))
	if end < 0 {
		return nil, nil, fmt.Errorf("frontmatter: missing closing `---`")
	}
	fm := after[:end]
	body := after[end+len("\n---\n"):]
	return fm, body, nil
}
