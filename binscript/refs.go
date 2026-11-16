package binscript

import (
	"fmt"
	"strings"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/yuin/gopher-lua/ast"
	luaparse "github.com/yuin/gopher-lua/parse"
)

// numericWeftFuncs are the weft functions that need a cron's numeric
// values, by their Lua names; bash spells them weft::<name>. The rest
// (latest, previous, describe, is_absent, has_changed) read text as well,
// and is_gt/is_lt test stdin, not a cron.
var numericWeftFuncs = map[string]bool{
	"avg": true, "max": true, "min": true, "sum": true, "stddev": true,
	"first": true, "count": true, "percentile": true, "delta": true,
	"slope": true, "rate": true, "is_above": true, "is_below": true,
	"is_rising": true, "is_falling": true, "is_outlier": true, "is_seasonal": true,
}

// NumericWeftFunc reports whether the weft function with this Lua name
// (bash: weft::<name>) needs numbers, so it refuses text crons.
func NumericWeftFunc(name string) bool { return numericWeftFuncs[name] }

// refSet collects cron names in first-seen order.
type refSet struct {
	names []string
	seen  map[string]bool
}

func (r *refSet) add(name string) {
	if r.seen == nil {
		r.seen = map[string]bool{}
	}
	if name != "" && !r.seen[name] {
		r.seen[name] = true
		r.names = append(r.names, name)
	}
}

// staticWord returns a word's text when it has no expansion, so save-time
// checks can read a name or a program only when it is spelled out.
func staticWord(w bash.Word) (string, bool) {
	var s string
	for _, p := range w.Parts {
		switch v := p.(type) {
		case *bash.LiteralPart:
			s += v.Value
		case *bash.SingleQuotedPart:
			s += v.Value
		default:
			return "", false
		}
	}
	return s, s != ""
}

// Refs lists the crons a /bin script reads with numeric weft
// functions, by literal name.
func Refs(language, source string) ([]string, error) {
	if language == "lua" {
		return LuaRefs(source)
	}
	script, err := bash.Parse(source)
	if err != nil {
		return nil, err
	}
	return BashRefs(script)
}

// BashRefs lists the crons script reads with numeric weft:: functions by
// literal name, including inside inline `lua '<program>'` stages. An
// inline program that doesn't parse is an error, reported like a /bin Lua
// script's. Names and programs built at run time can't be seen here; the
// functions check those when they run.
func BashRefs(script *bash.Script) ([]string, error) {
	var refs refSet
	var err error
	bash.Walk(script, func(n bash.Node) bool {
		if err != nil {
			return false
		}
		sc, ok := n.(*bash.SimpleCommand)
		if !ok || len(sc.Words) < 2 {
			return true
		}
		cmd := sc.Words[0].Literal()
		switch {
		case cmd == "lua":
			program, ok := staticWord(sc.Words[1])
			if !ok {
				return true
			}
			var names []string
			if names, err = LuaRefs(program); err != nil {
				err = fmt.Errorf("lua: %w", err)
				return false
			}
			for _, name := range names {
				refs.add(name)
			}
		case strings.HasPrefix(cmd, "weft::") && numericWeftFuncs[strings.TrimPrefix(cmd, "weft::")]:
			if name, ok := staticWord(sc.Words[1]); ok {
				refs.add(name)
			}
		}
		return true
	})
	return refs.names, err
}

// LuaRefs parses a Lua program and lists the crons it reads with numeric
// weft functions, weft.avg("errors", "1h") or weft["avg"]("errors", ...),
// by literal name.
func LuaRefs(source string) ([]string, error) {
	chunk, err := luaparse.Parse(strings.NewReader(source), "source")
	if err != nil {
		return nil, fmt.Errorf("syntax error: %w", err)
	}
	var refs refSet
	walkLuaStmts(chunk, func(e ast.Expr) {
		call, ok := e.(*ast.FuncCallExpr)
		if !ok || len(call.Args) == 0 {
			return
		}
		attr, ok := call.Func.(*ast.AttrGetExpr)
		if !ok {
			return
		}
		obj, ok := attr.Object.(*ast.IdentExpr)
		key, keyOK := attr.Key.(*ast.StringExpr)
		if !ok || !keyOK || obj.Value != "weft" || !numericWeftFuncs[key.Value] {
			return
		}
		if name, ok := call.Args[0].(*ast.StringExpr); ok {
			refs.add(name.Value)
		}
	})
	return refs.names, nil
}

// walkLuaStmts calls visit on every expression in stmts, nested ones
// included.
func walkLuaStmts(stmts []ast.Stmt, visit func(ast.Expr)) {
	exprs := func(es []ast.Expr) {
		for _, e := range es {
			walkLuaExpr(e, visit)
		}
	}
	for _, st := range stmts {
		switch s := st.(type) {
		case *ast.AssignStmt:
			exprs(s.Lhs)
			exprs(s.Rhs)
		case *ast.LocalAssignStmt:
			exprs(s.Exprs)
		case *ast.FuncCallStmt:
			walkLuaExpr(s.Expr, visit)
		case *ast.DoBlockStmt:
			walkLuaStmts(s.Stmts, visit)
		case *ast.WhileStmt:
			walkLuaExpr(s.Condition, visit)
			walkLuaStmts(s.Stmts, visit)
		case *ast.RepeatStmt:
			walkLuaExpr(s.Condition, visit)
			walkLuaStmts(s.Stmts, visit)
		case *ast.IfStmt:
			walkLuaExpr(s.Condition, visit)
			walkLuaStmts(s.Then, visit)
			walkLuaStmts(s.Else, visit)
		case *ast.NumberForStmt:
			exprs([]ast.Expr{s.Init, s.Limit, s.Step})
			walkLuaStmts(s.Stmts, visit)
		case *ast.GenericForStmt:
			exprs(s.Exprs)
			walkLuaStmts(s.Stmts, visit)
		case *ast.FuncDefStmt:
			walkLuaExpr(s.Func, visit)
		case *ast.ReturnStmt:
			exprs(s.Exprs)
		}
	}
}

func walkLuaExpr(e ast.Expr, visit func(ast.Expr)) {
	if e == nil {
		return
	}
	visit(e)
	switch x := e.(type) {
	case *ast.FuncCallExpr:
		walkLuaExpr(x.Func, visit)
		walkLuaExpr(x.Receiver, visit)
		for _, a := range x.Args {
			walkLuaExpr(a, visit)
		}
	case *ast.AttrGetExpr:
		walkLuaExpr(x.Object, visit)
		walkLuaExpr(x.Key, visit)
	case *ast.TableExpr:
		for _, f := range x.Fields {
			walkLuaExpr(f.Key, visit)
			walkLuaExpr(f.Value, visit)
		}
	case *ast.FunctionExpr:
		walkLuaStmts(x.Stmts, visit)
	case *ast.LogicalOpExpr:
		walkLuaExpr(x.Lhs, visit)
		walkLuaExpr(x.Rhs, visit)
	case *ast.RelationalOpExpr:
		walkLuaExpr(x.Lhs, visit)
		walkLuaExpr(x.Rhs, visit)
	case *ast.StringConcatOpExpr:
		walkLuaExpr(x.Lhs, visit)
		walkLuaExpr(x.Rhs, visit)
	case *ast.ArithmeticOpExpr:
		walkLuaExpr(x.Lhs, visit)
		walkLuaExpr(x.Rhs, visit)
	case *ast.UnaryMinusOpExpr:
		walkLuaExpr(x.Expr, visit)
	case *ast.UnaryNotOpExpr:
		walkLuaExpr(x.Expr, visit)
	case *ast.UnaryLenOpExpr:
		walkLuaExpr(x.Expr, visit)
	}
}
