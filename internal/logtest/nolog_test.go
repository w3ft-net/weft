package logtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Operator tools print to a terminal, with no request behind the output, so
// the stdlib logger is the right tool there.
var terminalTools = []string{
	"cmd/weft-admin", "cmd/weft-demo-seed", "cmd/docgen", "cmd/loadtest",
	"cmd/loom-producer", "cmd/weft-migrate", "cmd/weft",
	"bench", "integration", "docgen", "scripts",
}

// TestNoStdlibLogPrintsOutsideTerminalTools keeps trace IDs automatic: a
// stdlib log.Printf has no context to read one from, so only the SDK
// (ctx-aware, through the SDK's slog handler) may write a server log line.
func TestNoStdlibLogPrintsOutsideTerminalTools(t *testing.T) {
	root := filepath.Join("..", "..")
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "node_modules" || rel == "frontend" || strings.HasPrefix(rel, ".") && rel != "." {
				return filepath.SkipDir
			}
			for _, p := range terminalTools {
				if rel == p {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return nil
		}
		name := ""
		for _, im := range f.Imports {
			if p, _ := strconv.Unquote(im.Path.Value); p == "log" {
				name = "log"
				if im.Name != nil {
					name = im.Name.Name
				}
			}
		}
		if name == "" {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == name && strings.HasPrefix(sel.Sel.Name, "Print") {
				found = append(found, rel)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		counts := map[string]int{}
		for _, f := range found {
			counts[f]++
		}
		var b strings.Builder
		for f, n := range counts {
			b.WriteString("\n  " + f + ": " + strconv.Itoa(n))
		}
		t.Errorf("%d stdlib log.Print* calls remain in %d files; use weftsdk.Infof/Warnf/Errorf with the request's ctx:%s", len(found), len(counts), b.String())
	}
}
