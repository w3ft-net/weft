package weft_test

import (
	"fmt"
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
			bin, subcmd, flags, err := parseSystemdExecStart(svc)
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
			problems, err := unitFlagProblems(subcmd, flags, pkg)
			if err != nil {
				t.Fatalf("scan flags in %s: %v", pkg, err)
			}
			for _, p := range problems {
				t.Error(p)
			}
		})
	}
}

// unitFlagProblems returns one message per flag a unit passes that its binary
// does not define. With a subcommand (`weft-shard export --db=...`) the flags
// are checked against that subcommand's own flag.NewFlagSet alone, because
// that is where the binary parses them: a flag defined only for the subcommand
// must not satisfy a top-level use, nor the reverse. An ExecStart word that
// names no FlagSet in the package is not a subcommand and the check falls back
// to the top-level flags, as it always did.
func unitFlagProblems(subcmd string, flags []string, pkg string) ([]string, error) {
	var (
		defined map[string]struct{}
		where   = pkg
		hint    = `register it via flag.X("%s", ...) in main.go`
	)
	if subcmd != "" {
		subs, err := goSubcommandFlagsIn(pkg)
		if err != nil {
			return nil, err
		}
		if set, ok := subs[subcmd]; ok {
			defined = set
			where = pkg + " subcommand " + subcmd
			hint = `register it on the "` + subcmd + `" flag.NewFlagSet (fs.X("%s", ...))`
		}
	}
	if defined == nil {
		var err error
		if defined, err = goFlagsDefinedIn(pkg); err != nil {
			return nil, err
		}
	}
	var problems []string
	for _, f := range flags {
		if _, ok := defined[f]; !ok {
			problems = append(problems, fmt.Sprintf("ExecStart passes --%s but %s does not define it. The binary will crash on boot with `flag provided but not defined: -%s`. Either remove --%s from the unit or "+hint+".",
				f, where, f, f, f))
		}
	}
	return problems, nil
}

// parseSystemdExecStart reads a unit file and returns the absolute
// binary path, the subcommand (the first word after the binary that is
// not a --flag, "" if none), and the list of --flag names found in its
// ExecStart. Handles line continuations: lines ending with `\` are
// concatenated onto the ExecStart string. Stops at the first ExecStart
// (our units only have one).
func parseSystemdExecStart(path string) (string, string, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", nil, err
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
		return "", "", nil, nil
	}
	fields := strings.Fields(exec.String())
	if len(fields) == 0 {
		return "", "", nil, nil
	}
	bin := fields[0]
	var subcmd string
	var flags []string
	for _, f := range fields[1:] {
		if !strings.HasPrefix(f, "--") {
			if subcmd == "" {
				subcmd = f
			}
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
	return bin, subcmd, flags, nil
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
	flagNames, err := flagsDefinedInDir(pkgDir, false)
	if err != nil {
		return nil, err
	}
	// A thin main hands its process to a bootstrap package (app, app/apiapp)
	// that defines the flags on a *flag.FlagSet it is given.
	dirs, err := bootstrapDirs(pkgDir)
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		more, err := flagsDefinedInDir(dir, true)
		if err != nil {
			return nil, err
		}
		for name := range more {
			flagNames[name] = struct{}{}
		}
	}
	return flagNames, nil
}

// bootstrapDirs are the directories of the app/... packages pkgDir imports.
func bootstrapDirs(pkgDir string) ([]string, error) {
	const prefix = "github.com/graybeardcorp/weft/"
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, pkgDir, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var dirs []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, imp := range file.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil || !strings.HasPrefix(path, prefix+"app") {
					continue
				}
				if dir := strings.TrimPrefix(path, prefix); !seen[dir] {
					seen[dir] = true
					dirs = append(dirs, dir)
				}
			}
		}
	}
	return dirs, nil
}

// flagsDefinedInDir walks one directory. anyReceiver also accepts a flag
// registered on any variable (a *flag.FlagSet parameter), not just the flag
// package's own functions.
func flagsDefinedInDir(pkgDir string, anyReceiver bool) (map[string]struct{}, error) {
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
				if !ok || (ident.Name != "flag" && !(anyReceiver && flagDefiningMethods[sel.Sel.Name])) {
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

// flagDefiningMethods are the flag.FlagSet methods that register a flag; the
// name is the first string argument (second for the *Var forms). Methods like
// Set or Lookup also take a string but do not define anything.
var flagDefiningMethods = map[string]bool{
	"String": true, "Int": true, "Int64": true, "Uint": true, "Uint64": true,
	"Bool": true, "Float64": true, "Duration": true, "Func": true, "BoolFunc": true,
	"StringVar": true, "IntVar": true, "Int64Var": true, "UintVar": true, "Uint64Var": true,
	"BoolVar": true, "Float64Var": true, "DurationVar": true, "Var": true, "TextVar": true,
}

// goSubcommandFlagsIn AST-walks pkgDir and returns, per subcommand, the flags
// registered on its flag.NewFlagSet("<subcommand>", ...). Within each function it
// binds the variable a NewFlagSet is assigned to, then collects the literal flag
// names registered on that variable, so two subcommands in one package stay
// separate and a flag set in one function cannot claim another's variable.
func goSubcommandFlagsIn(pkgDir string) (map[string]map[string]struct{}, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, pkgDir, nil, 0)
	if err != nil {
		return nil, err
	}
	stringArg := func(call *ast.CallExpr, idx int) (string, bool) {
		if len(call.Args) <= idx {
			return "", false
		}
		lit, ok := call.Args[idx].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(lit.Value)
		return s, err == nil
	}
	out := map[string]map[string]struct{}{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				sets := map[string]string{} // FlagSet variable → subcommand name
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					as, ok := n.(*ast.AssignStmt)
					if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
						return true
					}
					lhs, ok := as.Lhs[0].(*ast.Ident)
					call, ok2 := as.Rhs[0].(*ast.CallExpr)
					if !ok || !ok2 {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "NewFlagSet" {
						return true
					}
					if pkgIdent, ok := sel.X.(*ast.Ident); !ok || pkgIdent.Name != "flag" {
						return true
					}
					if name, ok := stringArg(call, 0); ok {
						sets[lhs.Name] = name
						if out[name] == nil {
							out[name] = map[string]struct{}{}
						}
					}
					return true
				})
				if len(sets) == 0 {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || !flagDefiningMethods[sel.Sel.Name] {
						return true
					}
					recv, ok := sel.X.(*ast.Ident)
					if !ok {
						return true
					}
					subcmd, ok := sets[recv.Name]
					if !ok {
						return true
					}
					idx := 0
					if strings.HasSuffix(sel.Sel.Name, "Var") {
						idx = 1
					}
					if name, ok := stringArg(call, idx); ok {
						out[subcmd][name] = struct{}{}
					}
					return true
				})
			}
		}
	}
	return out, nil
}
