package weft_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestServiceUnitFlagsAreDefined cross-checks every --flag passed by
// the systemd units in deploy/*.service against the corresponding
// cmd/* binary's flag.X() registrations. The integration suite runs
// binaries via deploy/entrypoint.sh, which only ever passes --port,
// so flags that exist only in the systemd units (the prod entry
// point) slip through every other test we have.
//
// 2026-05-03: deploy/weft-compute.service grew --parallelism=4 ahead
// of the streaming work, but cmd/compute/main.go didn't define it,
// and every weft-compute boot crashed with "flag provided but not
// defined: -parallelism" — the compute target group went unhealthy
// in prod with no test in the suite that could have caught it.
func TestServiceUnitFlagsAreDefined(t *testing.T) {
	services, err := filepath.Glob("deploy/*.service")
	if err != nil {
		t.Fatal(err)
	}
	if len(services) == 0 {
		t.Fatal("no deploy/*.service files found — wrong working dir?")
	}

	for _, svc := range services {
		svc := svc
		t.Run(filepath.Base(svc), func(t *testing.T) {
			bin, flags, err := parseSystemdExecStart(svc)
			if err != nil {
				t.Fatalf("parse %s: %v", svc, err)
			}
			if bin == "" {
				t.Skip("no ExecStart line")
			}
			pkg := goPackageForBinary(bin)
			if pkg == "" {
				t.Skipf("ExecStart=%s is not a Go binary in cmd/", bin)
			}
			defined, err := goFlagsDefinedIn(pkg)
			if err != nil {
				t.Fatalf("scan flags in %s: %v", pkg, err)
			}
			for _, f := range flags {
				if _, ok := defined[f]; !ok {
					t.Errorf("ExecStart passes --%s but %s does not define it. The binary will crash on boot with `flag provided but not defined: -%s`. Either remove --%s from the unit or register it via flag.X(\"%s\", ...) in main.go.",
						f, pkg, f, f, f)
				}
			}
		})
	}
}

// parseSystemdExecStart reads a unit file and returns the absolute
// binary path plus the list of --flag names found in its ExecStart.
// Handles line continuations: lines ending with `\` are concatenated
// onto the ExecStart string. Stops at the first ExecStart (our units
// only have one).
func parseSystemdExecStart(path string) (string, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	var exec strings.Builder
	inExec := false
	for _, line := range strings.Split(string(raw), "\n") {
		if !inExec {
			if !strings.HasPrefix(line, "ExecStart=") {
				continue
			}
			inExec = true
			line = strings.TrimPrefix(line, "ExecStart=")
		}
		trimmed := strings.TrimRight(line, " \t")
		if strings.HasSuffix(trimmed, `\`) {
			exec.WriteString(strings.TrimSuffix(trimmed, `\`))
			exec.WriteByte(' ')
			continue
		}
		exec.WriteString(trimmed)
		break
	}
	if !inExec {
		return "", nil, nil
	}
	fields := strings.Fields(exec.String())
	if len(fields) == 0 {
		return "", nil, nil
	}
	bin := fields[0]
	var flags []string
	for _, f := range fields[1:] {
		if !strings.HasPrefix(f, "--") {
			continue
		}
		name := strings.TrimPrefix(f, "--")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		if name == "" {
			continue
		}
		flags = append(flags, name)
	}
	return bin, flags, nil
}

// goPackageForBinary maps a deploy ExecStart path like
// "/opt/app/bin/weft-coordinator" to its source directory
// ("cmd/coordinator"). Returns "" for non-binary ExecStart entries
// (e.g., shell-script launchers).
func goPackageForBinary(bin string) string {
	base := filepath.Base(bin)
	if !strings.HasPrefix(base, "weft-") {
		return ""
	}
	pkg := filepath.Join("cmd", strings.TrimPrefix(base, "weft-"))
	info, err := os.Stat(pkg)
	if err != nil || !info.IsDir() {
		return ""
	}
	return pkg
}

// goFlagsDefinedIn AST-walks every .go file in pkgDir and returns
// the set of flag names registered via flag.X("name", ...) or
// flag.XVar(&dst, "name", ...). Only handles literal-string flag
// names — dynamic / computed names aren't worth the parser
// complexity, and our codebase doesn't use them.
func goFlagsDefinedIn(pkgDir string) (map[string]struct{}, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, pkgDir, nil, 0)
	if err != nil {
		return nil, err
	}
	flagNames := map[string]struct{}{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok || ident.Name != "flag" {
					return true
				}
				// flag.X("name", ...)        → name lives at args[0].
				// flag.XVar(&dst, "name",..) → name lives at args[1].
				idx := 0
				if strings.HasSuffix(sel.Sel.Name, "Var") {
					idx = 1
				}
				if len(call.Args) <= idx {
					return true
				}
				lit, ok := call.Args[idx].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				name, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				flagNames[name] = struct{}{}
				return true
			})
		}
	}
	return flagNames, nil
}
