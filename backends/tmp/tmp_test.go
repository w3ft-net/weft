package tmp

import (
	"errors"
	"slices"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	for in, want := range map[string]string{"a": "a", "/a": "a", "a/": "a", "/a/b/": "a/b", "a b/c.txt": "a b/c.txt", "tmp/x": "tmp/x"} {
		if got, err := NormalizePath(in); err != nil || got != want {
			t.Errorf("NormalizePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "/", "//", "a//b", "a/./b", "a/../b", "..", ".", "a\x00b"} {
		if got, err := NormalizePath(in); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("NormalizePath(%q) = %q, %v; want ErrInvalidPath", in, got, err)
		}
	}
	if d, err := NormalizeDir("/"); err != nil || d != "" {
		t.Errorf("NormalizeDir(/) = %q, %v", d, err)
	}
	if _, err := NormalizeDir("a/../b"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("NormalizeDir of a bad path: %v", err)
	}
}

func TestParentsAndName(t *testing.T) {
	if got := Parents("a/b/c"); !slices.Equal(got, []string{"a", "a/b"}) {
		t.Errorf("Parents = %v", got)
	}
	if got := Parents("a"); len(got) != 0 {
		t.Errorf("Parents of a top-level path = %v", got)
	}
	if n := (File{Path: "a/b/c.txt"}).Name(); n != "c.txt" {
		t.Errorf("Name = %q", n)
	}
	if n := (File{Path: "top"}).Name(); n != "top" {
		t.Errorf("Name = %q", n)
	}
}
