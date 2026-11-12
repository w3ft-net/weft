package tenancy_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Hosted weft shares one NAT instance among its coordinators rather than pay
// for a NAT gateway, and elects which coordinator routes through it. That is
// an accident of the hosted deployment: the open build is unaware of NAT
// entirely. The election lives in tenancy/hosted/nat, which the hosted adapter
// starts; no other Go source may name it, not even in a comment or a test.
var natWords = regexp.MustCompile(`\bNAT\b|(?i:nat-holder|NATElector|NAT_ROUTE_TABLE_ID)|ReplaceRoute|CreateRoute`)

// Directories where hosted-only code lives, and may speak NAT. Everything else,
// every package and every test, may not.
var natAllowed = []string{"tenancy/hosted", "frontend", "docs", "node_modules"}

func TestOpenTreeIsUnawareOfNAT(t *testing.T) {
	root := ".."
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ".claude" {
				return filepath.SkipDir
			}
			for _, a := range natAllowed {
				if rel == a {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || rel == "tenancy/nat_lint_test.go" {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			if w := natWords.FindString(line); w != "" {
				found = append(found, rel+":"+strconv.Itoa(i+1)+": "+w)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Errorf("NAT election is hosted-only (tenancy/hosted/nat), but the open tree names it; move it there or reword:\n  %s", strings.Join(found, "\n  "))
	}
}
