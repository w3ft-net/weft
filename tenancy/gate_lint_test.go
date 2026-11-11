package tenancy_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Directories that may consult the org and permission tables: the store that
// owns them and the hosted adapter that is the one reader of them.
var gateOwners = []string{"store", "tenancy/hosted"}

// Store methods that answer "may this user touch this?". Everything else asks
// the Scoper or the Authorizer.
var permissionReads = map[string]bool{
	"GetNamespacePermission":       true,
	"ListUserNamespacePermissions": true,
	"GetOrgPermission":             true,
	"ReadableNamespaces":           true,
	"IsOwner":                      true,
}

// Files that read a permission-shaped value for a reason other than gating.
// Each needs its reason.
var gateExceptions = map[string]string{
	"auth/session.go": "builds the session: IsOwner is identity for the UI (no handler gates on it) and ActiveOrgID is what Session.Actor() exposes as the partition",
}

// TestNoPermissionChecksOutsideTheGates enforces contract 8: access is decided
// by Scoper.ReadScope and Authorizer.Authorize. Code that reads the
// permission tables, or compares a member's role to owner, is making its own
// decision and would be a second, divergent implementation of the boundary.
func TestNoPermissionChecksOutsideTheGates(t *testing.T) {
	root := ".."
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
			for _, o := range gateOwners {
				if rel == o {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if _, ok := gateExceptions[rel]; ok {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && permissionReads[sel.Sel.Name] {
					found = append(found, rel+": calls "+sel.Sel.Name)
				}
			case *ast.SelectorExpr:
				// The session's org is read through Session.Actor().Partition().
				if n.Sel.Name == "ActiveOrgID" {
					found = append(found, rel+": reads Session.ActiveOrgID; use session.Org()")
				}
			case *ast.BinaryExpr:
				if n.Op == token.EQL || n.Op == token.NEQ {
					for _, e := range []ast.Expr{n.X, n.Y} {
						if sel, ok := e.(*ast.SelectorExpr); ok && sel.Sel.Name == "RoleOwner" {
							found = append(found, rel+": compares a role to RoleOwner")
						}
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(found)
	if len(found) > 0 {
		t.Errorf("permission decisions outside tenancy.Scoper/Authorizer; ask the seam instead:\n  %s", strings.Join(found, "\n  "))
	}
}
