package planner

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/graybeardcorp/warp/pkg/bash"
)

// sourceInfo describes the source command (cat/head/tail) at the start of a pipeline.
//
// Multi-arg `cat a.log b.log` (and the post-glob-expansion form of
// `cat *.log`) populates namespaces/services/kinds with the union; the
// planner emits `namespace IN (...) AND service IN (...) AND kind IN (...)`
// predicates. Empty slices mean warp.log (the all-sources wildcard) — no
// predicate at all on that column.
type sourceInfo struct {
	namespaces []string // namespace column values for SQL predicates
	services   []string // service column values for SQL predicates
	kinds      []string // kind column values for SQL predicates
	env        string   // env column value; shared across all args in one pipeline
	cmd        string   // "cat", "head", "tail", or "trace"
	limit      int      // for head/tail: line count (0 = unlimited for cat)
	follow     bool     // tail -f
	traceID    string   // trace_id predicate value; only set when cmd == "trace"
	// spansOnly is `trace <id> --spans`'s flag. The SQL/row data this
	// produces is identical to a bare `trace <id>` (see docs/plans/
	// span-display.md, "Phase 1 in detail") — the rollup is a pure
	// client-side reinterpretation of the same rows — so this field
	// exists only so the flag doesn't fail sourceInfo's argument-count
	// check below; nothing here changes the predicate or query shape.
	spansOnly bool
}

// stageInfo describes a single pipeline stage after the source command.
type stageInfo struct {
	cmd  string   // command name: "awk", "jq", "grep", "sed", "sort", "head", "tail", etc.
	args []string // command arguments (raw literal strings)
}

// pipelineInfo is the parsed representation of a weft shell pipeline.
type pipelineInfo struct {
	source sourceInfo
	stages []stageInfo
}

// parsePipeline parses a weft shell pipeline string into structured form.
func parsePipeline(pipeline, cwd string) (*pipelineInfo, error) {
	script, err := bash.Parse(pipeline)
	if err != nil {
		return nil, fmt.Errorf("parse pipeline: %w", err)
	}

	if len(script.Commands) == 0 {
		return nil, fmt.Errorf("empty pipeline")
	}
	list := script.Commands[0]
	if list.First == nil {
		return nil, fmt.Errorf("empty pipeline")
	}
	pipe := list.First

	cmds := pipe.Commands
	if len(cmds) == 0 {
		return nil, fmt.Errorf("empty pipeline")
	}

	// Extract source command (first in pipeline).
	srcCmd, ok := cmds[0].(*bash.SimpleCommand)
	if !ok {
		return nil, fmt.Errorf("expected simple command as pipeline source")
	}

	src, err := parseSource(srcCmd, cwd)
	if err != nil {
		return nil, err
	}

	// Extract remaining stages.
	var stages []stageInfo
	for _, cmd := range cmds[1:] {
		sc, ok := cmd.(*bash.SimpleCommand)
		if !ok {
			return nil, fmt.Errorf("expected simple command in pipeline stage")
		}
		stages = append(stages, parseStage(sc))
	}

	return &pipelineInfo{source: *src, stages: stages}, nil
}

// errLocalSourcePath is returned by parseSource when a cat/head/tail
// source argument addresses a non-log virtual-FS namespace (/bin,
// /cron.d, /etc, /tmp). Plan turns this into Local mode so warp+
// shellfs handle the read (and any redirects) at runtime instead of
// the path being routed off the source-app allowlist.
var errLocalSourcePath = fmt.Errorf("local source path")

// IsLocalFSPath reports whether p points at a shellfs namespace that
// is not a log query — /bin, /cron.d, /etc, /tmp. These are read
// from the FS at runtime, not pushed down as shard SQL. Exported so
// the coordinator's pipeline-rewrite pass can skip wrapping such
// reads in a callback (which would infinite-loop back through Plan).
func IsLocalFSPath(p string) bool {
	return strings.HasPrefix(p, "/bin/") ||
		strings.HasPrefix(p, "/cron.d/") ||
		strings.HasPrefix(p, "/etc/") ||
		strings.HasPrefix(p, "/tmp/")
}

// parseSource extracts the source command info from the first pipeline command.
func parseSource(cmd *bash.SimpleCommand, cwd string) (*sourceInfo, error) {
	if len(cmd.Words) == 0 {
		return nil, fmt.Errorf("empty source command")
	}

	name := cmd.Words[0].Literal()
	args := literalArgs(cmd.Words[1:])

	switch name {
	case "cat":
		if len(args) == 0 {
			return nil, fmt.Errorf("cat: missing pseudofile argument")
		}
		for i := range args {
			args[i] = resolveSourcePath(args[i], cwd)
		}
		for _, a := range args {
			if IsLocalFSPath(a) {
				return nil, errLocalSourcePath
			}
		}
		sf, err := extractServicesEnv(args, "cat")
		if err != nil {
			return nil, err
		}
		return &sourceInfo{namespaces: sf.namespaces, services: sf.services, kinds: sf.kinds, env: sf.env, cmd: "cat"}, nil

	case "head":
		limit := 10
		var files []string
		for i := 0; i < len(args); i++ {
			if args[i] == "-n" && i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil {
					limit = n
				}
				i++ // skip the number
			} else if strings.HasPrefix(args[i], "-") {
				if n, err := strconv.Atoi(args[i][1:]); err == nil {
					limit = n
				}
			} else {
				files = append(files, args[i])
			}
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("head: missing pseudofile argument")
		}
		for i := range files {
			files[i] = resolveSourcePath(files[i], cwd)
		}
		for _, f := range files {
			if IsLocalFSPath(f) {
				return nil, errLocalSourcePath
			}
		}
		sf, err := extractServicesEnv(files, "head")
		if err != nil {
			return nil, err
		}
		return &sourceInfo{namespaces: sf.namespaces, services: sf.services, kinds: sf.kinds, env: sf.env, cmd: "head", limit: limit}, nil

	case "tail":
		limit := 10
		var files []string
		follow := false
		for i := 0; i < len(args); i++ {
			if args[i] == "-f" {
				follow = true
			} else if args[i] == "-n" && i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil {
					limit = n
				}
				i++ // skip the number
			} else if strings.HasPrefix(args[i], "-") {
				if n, err := strconv.Atoi(args[i][1:]); err == nil {
					limit = n
				}
			} else {
				files = append(files, args[i])
			}
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("tail: missing pseudofile argument")
		}
		for i := range files {
			files[i] = resolveSourcePath(files[i], cwd)
		}
		for _, f := range files {
			if IsLocalFSPath(f) {
				return nil, errLocalSourcePath
			}
		}
		sf, err := extractServicesEnv(files, "tail")
		if err != nil {
			return nil, err
		}
		return &sourceInfo{namespaces: sf.namespaces, services: sf.services, kinds: sf.kinds, env: sf.env, cmd: "tail", limit: limit, follow: follow}, nil

	case "trace":
		// No path argument — a trace isn't scoped to one app, it fans out
		// across every source the customer can read. The argument is an
		// opaque correlation ID, not a pseudofile path, so it skips
		// resolveSourcePath/IsLocalFSPath entirely.
		//
		// --spans is stripped here (any position) so `trace <id> --spans`
		// doesn't trip the argument-count check below — the flag is
		// otherwise inert server-side; the client decides span-rollup
		// rendering entirely on its own from the pipeline text it already
		// has (see shell.RenderSpanRollupIfApplicable).
		var spansOnly bool
		var rest []string
		for _, a := range args {
			if a == "--spans" {
				spansOnly = true
				continue
			}
			rest = append(rest, a)
		}
		if len(rest) != 1 {
			return nil, fmt.Errorf("trace: expected exactly one trace id argument")
		}
		return &sourceInfo{cmd: "trace", traceID: rest[0], spansOnly: spansOnly}, nil

	default:
		// A leading command other than cat/head/tail can never
		// establish a source path/kind — grep, awk, jq, sed, etc. are
		// pushdown/compute stages, not sources. If it was still handed
		// a /log/ path directly (real-Unix habit: `grep pattern file`
		// instead of piping through a source stage first), say so
		// explicitly rather than falling back to the generic "unknown
		// source command" message, which callers key off of to silently
		// reclassify the whole pipeline as ErrNotPlannable and hand it
		// to plain warp execution — where it reads shellfs's LogMount
		// directly and returns zero rows with no error at all. See
		// docs/known-issues.md, "A pipeline whose first stage isn't
		// cat/head/tail silently returns nothing".
		for _, a := range args {
			if resolved := resolveSourcePath(a, cwd); strings.HasPrefix(resolved, "/log/") {
				return nil, fmt.Errorf("pipeline must start with cat, head, or tail to read a /log/ path — %q can't be the first stage; try: tail %s | %s ...", name, resolved, name)
			}
		}
		return nil, fmt.Errorf("unknown source command: %s", name)
	}
}

// extractServicesEnv extracts namespace/service/kind + env from a list
// of pseudofile arguments. warp.log (the all-sources wildcard) yields
// empty slices, which means no predicate on those columns — a fan-out
// across every source the customer can read. Values are deduplicated
// and sorted per column for stable SQL.
//
// 2-seg paths (/log/<ns>/<svc>.log) are rejected outright: every log
// stream lives under an explicit kind, so the legacy two-segment form
// is meaningless. The error points at the canonical 3-seg shape.
//
// env is shared across all args in one pipeline; mixing env-less and
// env-bearing paths (or two different envs) in a single cat is rejected
// because the planner emits one env predicate for the whole query.
type sourceFields struct {
	namespaces []string
	services   []string
	kinds      []string
	env        string
}

func extractServicesEnv(files []string, cmd string) (sourceFields, error) {
	seenNS := map[string]struct{}{}
	seenSvc := map[string]struct{}{}
	seenKind := map[string]struct{}{}
	var sf sourceFields
	for i, f := range files {
		ns, env, svc, kind := parsePseudofilePath(f)
		if ns != "" && svc != "" && kind == "" {
			return sourceFields{}, fmt.Errorf("%s: %q is missing a kind segment; the canonical form is /log/%s/%s/<kind>.log (e.g. main, heartbeats, metrics)", cmd, f, ns, svc)
		}
		if ns != "" {
			if _, ok := seenNS[ns]; !ok {
				seenNS[ns] = struct{}{}
				sf.namespaces = append(sf.namespaces, ns)
			}
		}
		if svc != "" {
			if _, ok := seenSvc[svc]; !ok {
				seenSvc[svc] = struct{}{}
				sf.services = append(sf.services, svc)
			}
		}
		if kind != "" {
			if _, ok := seenKind[kind]; !ok {
				seenKind[kind] = struct{}{}
				sf.kinds = append(sf.kinds, kind)
			}
		}
		if i == 0 {
			sf.env = env
		} else if env != sf.env {
			return sourceFields{}, fmt.Errorf("%s: cannot mix env=%q and env=%q in one pipeline", cmd, sf.env, env)
		}
	}
	if len(sf.namespaces) > 1 {
		strSliceSort(sf.namespaces)
	}
	if len(sf.services) > 1 {
		strSliceSort(sf.services)
	}
	if len(sf.kinds) > 1 {
		strSliceSort(sf.kinds)
	}
	return sf, nil
}

func strSliceSort(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// parseStage extracts a stage from a non-source pipeline command.
func parseStage(cmd *bash.SimpleCommand) stageInfo {
	if len(cmd.Words) == 0 {
		return stageInfo{}
	}
	name := cmd.Words[0].Literal()
	args := literalArgs(cmd.Words[1:])
	return stageInfo{cmd: name, args: args}
}

// resolveSourcePath turns a relative source argument into an absolute
// path against cwd, so `tail gamestate.log` typed from /log/vimgym/api
// reads /log/vimgym/api/gamestate.log instead of being mis-parsed as the
// namespace "gamestate". Absolute args are returned untouched.
//
// cwd "" or "/" means "no directory context" — a fresh shell sits at
// home ("/") and addresses logs lexically (`tail log/<ns>/<svc>/<kind>.log`,
// or the bare `warp.log` wildcard). Leaving the arg untouched in that case
// preserves those legacy forms exactly; resolution only kicks in once the
// client has cd'd into a real /log subdirectory.
func resolveSourcePath(arg, cwd string) string {
	if arg == "" || strings.HasPrefix(arg, "/") {
		return arg
	}
	if cwd == "" || cwd == "/" {
		return arg
	}
	return path.Join(cwd, arg)
}

// parsePseudofilePath extracts namespace, env, service, and kind from a
// /log/ path.
//
// Path layout (segments after /log/, .log suffix stripped):
//
//	1 seg → "<ns>"                       namespace wildcard (no svc/kind filter)
//	2 seg → "<ns>/<svc>"                 rejected by extractServicesEnv — kind is required
//	3 seg → "<ns>/<svc>/<kind>"          env-less stream
//	4 seg → "<ns>/<env>/<svc>/<kind>"    env-bearing stream
//
// The 3-vs-4 distinction is unambiguous because every stream carries an
// explicit kind (defaulting to "main") and SeenServices.encodeName splices
// env at position 1 only for env-bearing producer tokens. Reader JWTs
// span envs, so the path must self-disambiguate without caller context;
// segment count does exactly that. 5+ segments are rejected.
func parsePseudofilePath(filename string) (namespace, env, service, kind string) {
	filename = strings.TrimPrefix(filename, "/log/")
	filename = strings.TrimPrefix(filename, "log/")
	name := strings.TrimSuffix(filename, ".log")
	if name == "warp" {
		return "", "", "", ""
	}
	parts := strings.Split(name, "/")
	switch len(parts) {
	case 1:
		namespace = parts[0]
	case 2:
		namespace = parts[0]
		service = parts[1]
		// kind stays empty — caller turns this into the "missing kind" error.
	case 3:
		namespace = parts[0]
		service = parts[1]
		kind = parts[2]
	case 4:
		namespace = parts[0]
		env = parts[1]
		service = parts[2]
		kind = parts[3]
	default:
		// 5+ segments: no valid shape. Leave everything empty so the
		// caller's "missing kind" guard rejects it with a clear error.
		namespace = parts[0]
		service = strings.Join(parts[1:], "/")
	}
	return
}

// parseLineCount extracts the line count from head/tail arguments.
// Handles: -N, -n N, -nN. Returns 10 as the default.
func parseLineCount(args []string) int {
	n := 10
	for i := 0; i < len(args); i++ {
		if args[i] == "-n" && i+1 < len(args) {
			if v, err := strconv.Atoi(args[i+1]); err == nil {
				n = v
			}
			i++
		} else if strings.HasPrefix(args[i], "-n") {
			if v, err := strconv.Atoi(args[i][2:]); err == nil {
				n = v
			}
		} else if strings.HasPrefix(args[i], "-") && args[i] != "-f" {
			if v, err := strconv.Atoi(args[i][1:]); err == nil {
				n = v
			}
		}
	}
	return n
}

// literalArgs extracts literal strings from a slice of Words.
func literalArgs(words []bash.Word) []string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = w.Literal()
	}
	return out
}
