package logtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// StdlibLogPrints finds the stdlib log.Print* calls in the Go sources under
// root, outside tests and the exempt directories (paths relative to root, with
// forward slashes), as file → count. Server code writes its log lines through
// the SDK, which reads the trace ID from the request's context; a stdlib
// log.Printf has no context to read one from. Operator tools that print to a
// terminal, with no request behind the output, are the exempt ones.
func StdlibLogPrints(root string, exempt ...string) (map[string]int, error) {
	counts := map[string]int{}
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
			for _, p := range exempt {
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
				counts[rel]++
			}
			return true
		})
		return nil
	})
	return counts, err
}

// FormatStdlibLogPrints renders StdlibLogPrints' result for a test failure.
func FormatStdlibLogPrints(counts map[string]int) string {
	files := make([]string, 0, len(counts))
	total := 0
	for f, n := range counts {
		files = append(files, f)
		total += n
	}
	sort.Strings(files)
	var b strings.Builder
	b.WriteString(strconv.Itoa(total) + " stdlib log.Print* calls remain in " + strconv.Itoa(len(files)) +
		" files; use weftsdk.Infof/Warnf/Errorf with the request's ctx:")
	for _, f := range files {
		b.WriteString("\n  " + f + ": " + strconv.Itoa(counts[f]))
	}
	return b.String()
}
