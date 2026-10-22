package main

import (
	"strings"
	"testing"
)

func mkDoc(path, body string) *doc { return &doc{path: path, body: []byte(body)} }

func TestResolveIncludes_SplicesSectionBody(t *testing.T) {
	src := mkDoc("docs/manual/a.md", "# T\n\n## One\n\nfirst\n\n### Sub\n\nnested\n\n## Two\n\nsecond\n")
	dst := mkDoc("docs/shell/b.md", "## HEAD\n\n{{include ../manual/a.md#one}}\n\ntail\n")
	if err := resolveIncludes([]*doc{src, dst}); err != nil {
		t.Fatal(err)
	}
	got := string(dst.body)
	for _, want := range []string{"## HEAD", "first", "### Sub", "nested", "tail"} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"## One", "second", "{{include"} {
		if strings.Contains(got, bad) {
			t.Errorf("body should not contain %q:\n%s", bad, got)
		}
	}
}

func TestResolveIncludes_IgnoresHashesInCodeFences(t *testing.T) {
	src := mkDoc("a.md", "## One\n\n```\n# not a heading\n```\n\nafter fence\n\n## Two\n")
	dst := mkDoc("b.md", "{{include a.md#one}}\n")
	if err := resolveIncludes([]*doc{src, dst}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dst.body), "after fence") {
		t.Errorf("section cut short at a # inside a code fence:\n%s", dst.body)
	}
}

func TestResolveIncludes_RetargetsLinks(t *testing.T) {
	src := mkDoc("docs/manual/a.md", "## One\n\nsee [crons](crons.md) and [below](#two) and [web](https://x.io/y)\n")
	dst := mkDoc("docs/shell/b.md", "{{include ../manual/a.md#one}}\n")
	if err := resolveIncludes([]*doc{src, dst}); err != nil {
		t.Fatal(err)
	}
	got := string(dst.body)
	for _, want := range []string{"](../manual/crons.md)", "](../manual/a.md#two)", "](https://x.io/y)"} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %q:\n%s", want, got)
		}
	}
}

func TestResolveIncludes_Errors(t *testing.T) {
	for name, tc := range map[string][]*doc{
		"missing doc":     {mkDoc("b.md", "{{include nope.md#x}}\n")},
		"missing section": {mkDoc("a.md", "## One\n"), mkDoc("b.md", "{{include a.md#two}}\n")},
		"cycle":           {mkDoc("a.md", "## One\n\n{{include b.md#two}}\n"), mkDoc("b.md", "## Two\n\n{{include a.md#one}}\n")},
	} {
		if err := resolveIncludes(tc); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestResolveIncludes_Nested(t *testing.T) {
	a := mkDoc("a.md", "## One\n\nleaf\n")
	b := mkDoc("b.md", "## Two\n\n{{include a.md#one}}\n")
	c := mkDoc("c.md", "{{include b.md#two}}\n")
	if err := resolveIncludes([]*doc{a, b, c}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(c.body), "leaf") {
		t.Errorf("nested include not expanded:\n%s", c.body)
	}
}
