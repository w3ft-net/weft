// Package logreads works out, from a script's text alone, which namespaces' logs
// it reads — or says why it can't.
//
// Crons run as the system identity, which reads every namespace, so what a cron
// may read cannot be enforced as it runs. It is decided when the cron is saved:
// the saver must be allowed to read everything the script reads. That only
// works if "everything the script reads" can be known without running it, so
// anything that could name a log path but isn't fixed by the script's text — a
// variable, a command substitution, eval, a loop variable, a cd into a computed
// directory — is refused rather than guessed at.
//
// The analysis is deliberately conservative. It may refuse a script that is in
// fact harmless; it must not accept one that reads a namespace it didn't report.
package logreads

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/weft/tracecmd"
)

// Env is what resolving a script needs to know about the org.
type Env struct {
	// Bin returns the source of /bin/<name>, whether it is a Lua script, and
	// whether such a script exists.
	Bin func(name string) (source string, lua bool, ok bool)
	// Namespaces lists every namespace in the org. Globs match against it, and
	// "every namespace" means exactly these.
	Namespaces []string
	// RequireBinPath refuses a /bin script called by its bare name, in the
	// script and in every script it runs: a cron names what it runs as
	// /bin/<name>, so the scripts it depends on can all be found.
	RequireBinPath bool
}

// Reads is the answer: the namespaces a script reads and the /bin scripts it
// runs, directly or through other scripts, both sorted.
type Reads struct {
	Namespaces []string
	Bins       []string
}

// UnresolvableError says what stopped the analysis.
type UnresolvableError struct{ Reason string }

func (e *UnresolvableError) Error() string { return e.Reason }

const maxDepth = 8 // how deep scripts may call scripts

// Analyze reports what script reads. It returns an *UnresolvableError for a
// script it can't resolve, and a plain error for one that doesn't parse.
func Analyze(script string, env Env) (*Reads, error) {
	a := &analyzer{env: env, ns: map[string]bool{}, bins: map[string]bool{}}
	ast, err := bash.Parse(script)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	a.script(ast, &frame{cwd: "/"}, true)
	if a.err != nil {
		return nil, a.err
	}
	return &Reads{Namespaces: sortedKeys(a.ns), Bins: sortedKeys(a.bins)}, nil
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

type analyzer struct {
	env   Env
	ns    map[string]bool
	bins  map[string]bool
	stack []string // scripts being analyzed, to cut cycles
	err   error
}

// frame is the state of one script: where it is, and what it was called with.
type frame struct {
	cwd        string
	cwdUnknown bool
	args       []arg // positional parameters $1, $2, ...
}

// arg is one positional parameter: a known literal, or unknown.
type arg struct {
	val   string
	known bool
	why   string // when unknown: what it was computed from
}

func (a *analyzer) fail(format string, v ...any) {
	if a.err == nil {
		a.err = &UnresolvableError{Reason: fmt.Sprintf(format, v...)}
	}
}

// script walks a sequence of statements. top is true only for the outermost
// statements of a script, the one place a cd is followed; a cd anywhere else
// leaves the directory unknown.
func (a *analyzer) script(s *bash.Script, f *frame, top bool) {
	if s == nil {
		return
	}
	for _, l := range s.Commands {
		if a.err != nil {
			return
		}
		a.list(l, f, top)
	}
}

func (a *analyzer) list(l *bash.List, f *frame, top bool) {
	// `cd X` or `cd X && ...` as a statement of its own moves the directory for
	// the rest of the script. In any other position it might or might not run.
	if top && l.First != nil && len(l.First.Commands) == 1 {
		if sc, ok := l.First.Commands[0].(*bash.SimpleCommand); ok && a.isDirChange(sc, f) {
			a.changeDir(sc, f)
			for _, c := range l.Rest {
				a.pipeline(c.Pipeline, f)
			}
			return
		}
	}
	a.pipeline(l.First, f)
	for _, c := range l.Rest {
		a.pipeline(c.Pipeline, f)
	}
}

func (a *analyzer) isDirChange(sc *bash.SimpleCommand, f *frame) bool {
	if len(sc.Words) == 0 {
		return false
	}
	name, ok := a.literal(sc.Words[0], f)
	return ok && (name == "cd" || name == "pushd" || name == "popd")
}

func (a *analyzer) changeDir(sc *bash.SimpleCommand, f *frame) {
	name, _ := a.literal(sc.Words[0], f)
	a.subs(sc, f)
	if name != "cd" || len(sc.Words) != 2 {
		f.cwdUnknown = true
		return
	}
	target, ok := a.literal(sc.Words[1], f)
	if !ok || target == "" || target == "-" || strings.HasPrefix(target, "~") || hasGlob(sc.Words[1]) {
		f.cwdUnknown = true
		return
	}
	if f.cwdUnknown {
		if strings.HasPrefix(target, "/") {
			f.cwd, f.cwdUnknown = path.Clean(target), false
		}
		return
	}
	f.cwd = abs(f.cwd, target)
}

func (a *analyzer) pipeline(p *bash.Pipeline, f *frame) {
	if p == nil {
		return
	}
	for _, c := range p.Commands {
		if a.err != nil {
			return
		}
		a.command(c, f)
	}
}

func (a *analyzer) command(c bash.Command, f *frame) {
	switch n := c.(type) {
	case *bash.SimpleCommand:
		a.simple(n, f)
	case *bash.Subshell:
		a.nested(n.Body, f)
		a.redirects(n.Redirects, f)
	case *bash.Group:
		a.nested(n.Body, f)
		a.redirects(n.Redirects, f)
	case *bash.IfClause:
		a.nested(n.Condition, f)
		a.nested(n.Body, f)
		for _, e := range n.Elifs {
			a.nested(e.Condition, f)
			a.nested(e.Body, f)
		}
		a.nested(n.ElseBody, f)
		a.redirects(n.Redirects, f)
	case *bash.WhileClause:
		a.nested(n.Condition, f)
		a.nested(n.Body, f)
		a.redirects(n.Redirects, f)
	case *bash.ForClause:
		for _, w := range n.Words {
			a.subsOfWord(w, f)
			a.record(w, f, false)
		}
		a.nested(n.Body, f)
		a.redirects(n.Redirects, f)
	case *bash.CaseClause:
		a.subsOfWord(n.Word, f)
		for _, it := range n.Items {
			a.nested(it.Body, f)
		}
		a.redirects(n.Redirects, f)
	case *bash.FuncDef:
		a.command(n.Body, f)
	default:
		a.fail("unsupported construct %T", c)
	}
}

// nested walks statements that may or may not run, or run elsewhere: a cd in
// them cannot be followed.
func (a *analyzer) nested(s *bash.Script, f *frame) {
	if s == nil {
		return
	}
	inner := *f
	a.script(s, &inner, false)
	if inner.cwd != f.cwd || inner.cwdUnknown {
		f.cwdUnknown = true
	}
}

func (a *analyzer) redirects(rs []bash.Redirect, f *frame) {
	for _, r := range rs {
		a.subsOfWord(r.Target, f)
		switch r.Op {
		case "<<", "<<-", "<<<", ">&", "<&", "2>&1":
			continue // data or a file descriptor, not a path
		}
		reading := r.Op == "<"
		a.record(r.Target, f, false)
		if reading {
			a.requireResolved(r.Target, f, "input redirected from")
		}
	}
}

// subs walks the command substitutions inside a simple command's words.
func (a *analyzer) subs(sc *bash.SimpleCommand, f *frame) {
	for _, as := range sc.Assigns {
		a.subsOfWord(as.Value, f)
	}
	for _, w := range sc.Words {
		a.subsOfWord(w, f)
	}
}

func (a *analyzer) subsOfWord(w bash.Word, f *frame) {
	var visit func(parts []bash.WordPart)
	visit = func(parts []bash.WordPart) {
		for _, p := range parts {
			switch v := p.(type) {
			case *bash.CommandSub:
				a.nested(v.Body, f)
			case *bash.DoubleQuotedPart:
				visit(v.Parts)
			case *bash.ParamExpansion:
				if v.Arg != nil {
					a.subsOfWord(*v.Arg, f)
				}
			}
		}
	}
	visit(w.Parts)
}

// piece is a fragment of a word after expansion: text with the quoting that
// matters for globbing and word splitting, or something not known statically.
type piece struct {
	text   string
	quoted bool   // inside quotes: no glob, no word splitting
	dyn    bool   // an expansion whose value isn't known
	why    string // for dyn: what it is, for the error message
}

func (a *analyzer) pieces(w bash.Word, f *frame) []piece {
	var out []piece
	var visit func(parts []bash.WordPart, quoted bool)
	visit = func(parts []bash.WordPart, quoted bool) {
		for _, p := range parts {
			switch v := p.(type) {
			case *bash.LiteralPart:
				out = append(out, piece{text: v.Value, quoted: quoted})
			case *bash.SingleQuotedPart:
				out = append(out, piece{text: v.Value, quoted: true})
			case *bash.DoubleQuotedPart:
				visit(v.Parts, true)
			case *bash.ParamExpansion:
				out = append(out, a.param(v.Name, v.Op != "" || v.Length, quoted, f))
			case *bash.SpecialVar:
				out = append(out, a.param(v.Name, false, quoted, f))
			case *bash.CommandSub:
				out = append(out, piece{dyn: true, quoted: quoted, why: "a command substitution"})
			case *bash.ArithExpansion:
				out = append(out, piece{dyn: true, quoted: quoted, why: "an arithmetic expansion"})
			default:
				out = append(out, piece{dyn: true, quoted: quoted, why: fmt.Sprintf("%T", p)})
			}
		}
	}
	visit(w.Parts, false)
	return out
}

// param resolves a positional parameter to the literal it was called with if it
// is known; every other parameter is not.
func (a *analyzer) param(name string, modified, quoted bool, f *frame) piece {
	if !modified && len(name) == 1 && name[0] >= '1' && name[0] <= '9' {
		i := int(name[0] - '1')
		if i >= len(f.args) {
			return piece{text: "", quoted: true} // unset: expands to nothing
		}
		if f.args[i].known {
			v := f.args[i].val
			// A value with spaces or glob characters would split or glob
			// when unquoted, so it is only known when quoted.
			if !quoted && strings.ContainsAny(v, " \t\n*?[{") {
				return piece{dyn: true, quoted: quoted, why: "$" + name}
			}
			return piece{text: v, quoted: true}
		}
		return piece{dyn: true, quoted: quoted, why: "$" + name + " (passed " + f.args[i].why + ")"}
	}
	return piece{dyn: true, quoted: quoted, why: "$" + name}
}

// literal returns the word's text if nothing in it is unknown.
func (a *analyzer) literal(w bash.Word, f *frame) (string, bool) {
	var sb strings.Builder
	for _, p := range a.pieces(w, f) {
		if p.dyn {
			return "", false
		}
		sb.WriteString(p.text)
	}
	return sb.String(), true
}

func hasGlob(w bash.Word) bool {
	for _, p := range w.Parts {
		if l, ok := p.(*bash.LiteralPart); ok && strings.ContainsAny(l.Value, "*?[") {
			return true
		}
	}
	return false
}

// splits reports whether the word could expand to more or fewer words than
// one: an expansion outside double quotes does.
func (a *analyzer) splits(w bash.Word, f *frame) bool {
	for _, p := range a.pieces(w, f) {
		if p.dyn && !p.quoted {
			return true
		}
	}
	return false
}

func abs(cwd, p string) string {
	if strings.HasPrefix(p, "/") {
		return path.Clean(p)
	}
	return path.Clean(cwd + "/" + p)
}

// record notes the namespaces a word names when it is a path under /log.
// recursive is for commands that descend into directories, which read all of
// /log when pointed at / or /log.
func (a *analyzer) record(w bash.Word, f *frame, recursive bool) {
	ps := a.pieces(w, f)
	var sb strings.Builder
	for _, p := range ps {
		if p.dyn {
			break
		}
		sb.WriteString(p.text)
	}
	lit := sb.String()
	allLiteral := true
	for _, p := range ps {
		if p.dyn {
			allLiteral = false
		}
	}
	if !allLiteral || lit == "" || strings.HasPrefix(lit, "-") {
		return
	}
	if !strings.HasPrefix(lit, "/") && f.cwdUnknown {
		return // requireResolved decides what an unknown directory means
	}
	a.recordPath(abs(f.cwd, lit), recursive)
}

// recordPath classifies an absolute path.
func (a *analyzer) recordPath(p string, recursive bool) {
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if p == "/" {
		if recursive {
			a.all()
		}
		return
	}
	if strings.ContainsAny(segs[0], "*?[") {
		if ok, _ := path.Match(segs[0], "log"); !ok {
			return
		}
	} else if segs[0] != "log" {
		return
	}
	if len(segs) == 1 {
		a.all() // /log itself: listing it names every namespace
		return
	}
	ns := segs[1]
	switch {
	case strings.ContainsAny(ns, "{}"):
		a.fail("a brace expansion in the log path %q can't be resolved", p)
	case strings.ContainsAny(ns, "*?["):
		// A pattern matching no namespace today reads nothing.
		for _, n := range a.env.Namespaces {
			if ok, _ := path.Match(ns, n); ok {
				a.ns[n] = true
			}
		}
	default:
		a.ns[ns] = true
	}
}

func (a *analyzer) all() {
	for _, n := range a.env.Namespaces {
		a.ns[n] = true
	}
}

// mayBeLog reports whether a word with unknown parts could expand to a path
// under /log, judging by the known text before the first unknown part.
func (a *analyzer) mayBeLog(w bash.Word, f *frame) (bool, string) {
	var sb strings.Builder
	why := ""
	for _, p := range a.pieces(w, f) {
		if p.dyn {
			why = p.why
			break
		}
		sb.WriteString(p.text)
	}
	prefix := sb.String()
	if why == "" {
		return false, ""
	}
	if strings.HasPrefix(prefix, "-") {
		return false, why // an option, whatever follows
	}
	var full string
	switch {
	case strings.HasPrefix(prefix, "/"):
		full = prefix
	case f.cwdUnknown:
		return true, why
	default:
		full = f.cwd
		if !strings.HasSuffix(full, "/") {
			full += "/"
		}
		full += prefix
	}
	return strings.HasPrefix("/log", full) || strings.HasPrefix(full, "/log"), why
}

// requireResolved refuses a word that could be a log path but isn't fixed.
func (a *analyzer) requireResolved(w bash.Word, f *frame, what string) {
	if may, why := a.mayBeLog(w, f); may {
		a.fail("%s a path computed from %s can't be resolved to the logs it reads", what, why)
	}
}

// whyDynamic names what makes a word unknown, or "".
func (a *analyzer) whyDynamic(w bash.Word, f *frame) string {
	for _, p := range a.pieces(w, f) {
		if p.dyn {
			return p.why
		}
	}
	return ""
}

// operand is a word that a command will treat as a path: note the logs it
// names, and refuse it if it might name some that can't be told.
func (a *analyzer) operand(w bash.Word, f *frame, what string, recursive bool) {
	if prefix := a.leading(w, f); prefix != "" && !strings.HasPrefix(prefix, "/") && !strings.HasPrefix(prefix, "-") && f.cwdUnknown {
		a.fail("%s %q: the working directory is computed, so the path can't be resolved", what, prefix)
		return
	}
	a.record(w, f, recursive)
	if may, why := a.mayBeLog(w, f); may {
		a.fail("%s a path computed from %s can't be resolved to the logs it reads", what, why)
	}
}

// leading is the known text at the start of a word.
func (a *analyzer) leading(w bash.Word, f *frame) string {
	var sb strings.Builder
	for _, p := range a.pieces(w, f) {
		if p.dyn {
			break
		}
		sb.WriteString(p.text)
	}
	return sb.String()
}

// recordCwd notes the working directory itself as read, for the commands that
// act on it when given no path.
func (a *analyzer) recordCwd(what string, f *frame, recursive bool) {
	if f.cwdUnknown {
		a.fail("%s with no path acts on the working directory, which is computed", what)
		return
	}
	a.recordPath(f.cwd, recursive)
}

// Commands that can run code the analysis doesn't see.
var opaque = map[string]bool{
	"eval": true, "source": true, ".": true, "alias": true, "exec": true,
	"bash": true, "sh": true, "zsh": true, "command": true, "builtin": true,
	"env": true, "nohup": true, "time": true, "timeout": true, "watch": true,
}

// grammar describes how a command that reads files takes its arguments.
type grammar struct {
	valueOpts  map[string]bool // options that consume the next word
	program    bool            // the first operand is a pattern or program, not a file
	progOpts   map[string]bool // options that supply the program instead
	pathOpts   map[string]bool // options whose value is a file
	recursive  bool            // always descends into directories
	recurFlags map[string]bool // flags that make it descend
	cwdDefault bool            // with no path, acts on the working directory
}

func set(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

// The shell supports a fixed set of options per command and rejects any
// other, so these tables are complete for what can run.
var grammars = map[string]grammar{
	"cat":   {},
	"less":  {},
	"wc":    {},
	"diff":  {},
	"comm":  {},
	"uniq":  {},
	"head":  {valueOpts: set("-n", "-c")},
	"tail":  {valueOpts: set("-n", "-c")},
	"paste": {valueOpts: set("-d")},
	"join":  {valueOpts: set("-t", "-1", "-2", "-a")},
	"sort":  {valueOpts: set("-k", "-t")},
	"cut":   {valueOpts: set("-d", "-f", "-c")},
	"grep":  {program: true, recurFlags: set("-r", "-R")},
	"sed":   {program: true, valueOpts: set("-e"), progOpts: set("-e")},
	"awk":   {program: true, valueOpts: set("-F", "-v", "-f"), progOpts: set("-f"), pathOpts: set("-f")},
	"jq":    {program: true},
	"ls":    {cwdDefault: true},
	"tree":  {recursive: true, cwdDefault: true},
	"cp":    {}, "mv": {}, "rm": {}, "touch": {}, "mkdir": {}, "tee": {},
	"vi": {}, "vim": {},
}

func (a *analyzer) simple(sc *bash.SimpleCommand, f *frame) {
	a.subs(sc, f)
	for _, as := range sc.Assigns {
		a.record(as.Value, f, false)
	}
	defer func() { a.redirects(sc.Redirects, f) }()
	if len(sc.Words) == 0 {
		return
	}
	name, ok := a.literal(sc.Words[0], f)
	if !ok {
		a.fail("the command name is computed (from %s), so what it reads can't be known", a.whyDynamic(sc.Words[0], f))
		return
	}
	args := sc.Words[1:]

	if opaque[name] {
		a.fail("%s can run code that can't be analyzed, so it isn't allowed in a cron or in a script a cron runs", name)
		return
	}
	switch name {
	case "cd", "pushd", "popd":
		f.cwdUnknown = true // a cd that isn't a plain statement: not followed
		return
	case "trace":
		a.trace(args, f)
		return
	case "xargs":
		a.xargs(args, f)
		return
	case "find":
		a.find(args, f)
		return
	}

	isBin := a.bin(name, args, f)
	if g, ok := grammars[name]; ok {
		a.pathCommand(name, g, args, f)
	} else if !isBin {
		// Not a command that reads files: only a literal /log path in it counts.
		for _, w := range args {
			a.record(w, f, false)
		}
	}
}

// pathCommand applies a command's grammar to its arguments.
func (a *analyzer) pathCommand(name string, g grammar, args []bash.Word, f *frame) {
	programPending := g.program
	recursive := g.recursive
	operands := 0
	endOpts := false
	skipNext, nextIsPath := false, false

	for _, w := range args {
		if a.err != nil {
			return
		}
		if skipNext {
			skipNext = false
			if a.splits(w, f) {
				a.fail("%s's option value, computed from %s, may expand to several words", name, a.whyDynamic(w, f))
				return
			}
			if nextIsPath {
				a.operand(w, f, name+" reading", false)
			}
			continue
		}
		prefix := a.leading(w, f)
		if !endOpts && strings.HasPrefix(prefix, "-") && len(prefix) > 1 {
			if prefix == "--" {
				endOpts = true
				continue
			}
			if a.splits(w, f) {
				a.fail("the option %q to %s is computed from %s and may add operands", prefix, name, a.whyDynamic(w, f))
				return
			}
			flag := prefix
			attached := false
			if strings.HasPrefix(prefix, "--") {
				if i := strings.Index(prefix, "="); i >= 0 {
					flag, attached = prefix[:i], true
				}
			} else if len(prefix) > 2 {
				flag, attached = prefix[:2], true
			}
			if g.recurFlags[flag] || (len(prefix) > 2 && !strings.HasPrefix(prefix, "--") && anyRecurFlag(g, prefix[1:])) {
				recursive = true
			}
			if g.valueOpts[flag] {
				if g.progOpts[flag] {
					programPending = false
				}
				if !attached {
					skipNext, nextIsPath = true, g.pathOpts[flag]
				}
			}
			continue
		}
		if programPending {
			programPending = false
			if a.splits(w, f) {
				a.fail("%s's pattern or program, computed from %s, may expand to several words", name, a.whyDynamic(w, f))
				return
			}
			continue
		}
		operands++
		a.operand(w, f, name+" reading", recursive)
	}
	if operands == 0 && (g.cwdDefault || recursive) && a.err == nil {
		a.recordCwd(name, f, recursive)
	}
}

func anyRecurFlag(g grammar, letters string) bool {
	for _, r := range letters {
		if g.recurFlags["-"+string(r)] {
			return true
		}
	}
	return false
}

// find takes paths first, then an expression whose words are not paths. It
// descends into everything it is given.
func (a *analyzer) find(args []bash.Word, f *frame) {
	paths := 0
	for _, w := range args {
		p := a.leading(w, f)
		if strings.HasPrefix(p, "-") || p == "(" || p == "!" {
			break
		}
		paths++
		a.operand(w, f, "find searching", true)
		if a.err != nil {
			return
		}
	}
	if paths == 0 {
		a.recordCwd("find", f, true)
	}
}

// trace reads the logs of the namespace it is given, or of every namespace.
func (a *analyzer) trace(args []bash.Word, f *frame) {
	lits := make([]string, 0, len(args))
	for _, w := range args {
		s, ok := a.literal(w, f)
		if !ok {
			a.fail("trace's arguments are computed from %s, so the namespace it reads can't be known", a.whyDynamic(w, f))
			return
		}
		lits = append(lits, s)
	}
	c, err := tracecmd.Parse(lits)
	if err != nil {
		return // a malformed trace fails when it runs and reads nothing
	}
	if c.Namespace != "" {
		a.ns[c.Namespace] = true
		return
	}
	a.all()
}

// xargs runs a command on words it reads from its input, so a command that
// takes paths would be handed paths no one wrote down.
func (a *analyzer) xargs(args []bash.Word, f *frame) {
	i := 0
	for i < len(args) {
		s, ok := a.literal(args[i], f)
		if !ok {
			a.fail("xargs runs a command computed from %s", a.whyDynamic(args[i], f))
			return
		}
		if strings.HasPrefix(s, "-") && len(s) > 1 {
			if s == "-n" || s == "-P" || s == "-I" {
				i++
			}
			i++
			continue
		}
		break
	}
	if i >= len(args) {
		return // a bare xargs runs echo
	}
	inner, _ := a.literal(args[i], f)
	if a.takesPaths(inner) {
		a.fail("xargs would feed paths from its input to %s, which can't be resolved to the logs it reads", inner)
	}
}

func (a *analyzer) takesPaths(name string) bool {
	if _, ok := grammars[name]; ok {
		return true
	}
	switch name {
	case "find", "trace", "xargs":
		return true
	}
	if a.env.Bin != nil {
		if _, _, ok := a.env.Bin(strings.TrimPrefix(name, "/bin/")); ok {
			return true
		}
	}
	return false
}

// bin runs the analysis into a /bin script the command names, binding the
// literal arguments it was called with to the script's positional parameters.
// It reports whether name is a /bin script.
func (a *analyzer) bin(name string, args []bash.Word, f *frame) bool {
	sname := name
	if strings.HasPrefix(name, "/bin/") {
		sname = strings.TrimPrefix(name, "/bin/")
	}
	if sname == "" || strings.Contains(sname, "/") || a.env.Bin == nil {
		return false
	}
	src, lua, ok := a.env.Bin(sname)
	if !ok {
		return false
	}
	if a.env.RequireBinPath && !strings.HasPrefix(name, "/bin/") {
		a.fail("%s is a /bin script; a cron calls it by its path, /bin/%s", sname, sname)
		return true
	}
	a.bins[sname] = true
	for _, w := range args {
		a.record(w, f, false)
	}
	if lua || slices.Contains(a.stack, sname) {
		return true // a Lua script reads only its input; a cycle is already being analyzed
	}
	if len(a.stack) >= maxDepth {
		a.fail("scripts call each other more than %d deep", maxDepth)
		return true
	}
	call := make([]arg, len(args))
	for i, w := range args {
		if v, ok := a.literal(w, f); ok {
			call[i] = arg{val: v, known: true}
		} else {
			call[i] = arg{why: a.whyDynamic(w, f)}
		}
	}
	ast, err := bash.Parse(src)
	if err != nil {
		a.fail("the script %s does not parse: %v", sname, err)
		return true
	}
	a.stack = append(a.stack, sname)
	a.script(ast, &frame{cwd: f.cwd, cwdUnknown: f.cwdUnknown, args: call}, true)
	a.stack = a.stack[:len(a.stack)-1]
	return true
}
