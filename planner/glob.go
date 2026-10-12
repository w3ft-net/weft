package planner

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/graybeardcorp/warp/pkg/bash"
)

// ExpandSourceGlobs rewrites a pipeline so that glob patterns in the
// source command's pseudofile arguments expand to the concrete app
// pseudofiles they match. Returns the unchanged input when no glob
// metacharacters are present in any source arg.
//
// Globs operate on the *kind segment* of the path inside one service
// directory: `cat /log/acme/prod/api/*.log` becomes
// `cat /log/acme/prod/api/main.log /log/acme/prod/api/metrics.log`.
// A glob that would read across services or namespaces (`cat *.log`
// from /, `cat /log/acme/prod/*.log`, `/log/acme/*/api/main.log`) is
// refused with an error that says so; `cat <ns>.log` is the way to
// read a whole namespace.
//
// Glob with no matches is an error (mirroring bash's `failglob`
// behavior — silently dropping it would surface as an empty result
// the user couldn't distinguish from "no rows" and is the wrong
// failure mode for an analyst's interactive shell).
// cwd resolves relative glob patterns (`cat *.log` from /log/vimgym/api
// → /log/vimgym/api/*.log) before matching, the same rule parseSource
// applies to concrete source args. cwd "" / "/" (home) leaves the
// pattern lexical, so an absolute `/log/<ns>/<env>/*.log` is unaffected.
func ExpandSourceGlobs(pipeline, cwd string, apps []string) (string, error) {
	if !containsGlobMeta(pipeline) {
		return pipeline, nil
	}
	script, err := bash.Parse(pipeline)
	if err != nil {
		// Let the planner surface the parse error; pass through.
		return pipeline, nil
	}
	if len(script.Commands) == 0 {
		return pipeline, nil
	}
	list := script.Commands[0]
	if list.First == nil || len(list.First.Commands) == 0 {
		return pipeline, nil
	}
	srcCmd, ok := list.First.Commands[0].(*bash.SimpleCommand)
	if !ok || len(srcCmd.Words) == 0 {
		return pipeline, nil
	}

	name := srcCmd.Words[0].Literal()
	if name != "cat" && name != "head" && name != "tail" {
		return pipeline, nil
	}

	// Walk the source's args; for each glob, expand and emit multiple
	// args in its place. Non-glob args (and flags like -n, -f) pass
	// through unchanged.
	var rewrittenArgs []string
	changed := false
	for _, w := range srcCmd.Words[1:] {
		arg := w.Literal()
		if !containsGlobMeta(arg) {
			rewrittenArgs = append(rewrittenArgs, arg)
			continue
		}
		matches, err := matchPseudofileGlob(resolveSourcePath(arg, cwd), apps)
		if errors.Is(err, errCrossSourceGlob) {
			return "", fmt.Errorf("%s: the glob %q is not allowed: it would read across sources. Glob the kinds inside one service (/log/<ns>/<env>/<svc>/*.log), or read a whole namespace with `%s <ns>.log`", name, arg, name)
		}
		if err != nil {
			return "", err
		}
		if len(matches) == 0 {
			return "", fmt.Errorf("%s: no pseudofiles match %s", name, arg)
		}
		rewrittenArgs = append(rewrittenArgs, matches...)
		changed = true
	}
	if !changed {
		return pipeline, nil
	}

	// Rebuild only the source command; keep the rest of the pipeline
	// (everything after the first `|`) verbatim. We slice the original
	// string at the position of the first pipe to preserve user
	// formatting for downstream stages.
	rest := ""
	if i := indexUnquotedPipe(pipeline); i >= 0 {
		// Preserve a single space before the pipe; downstream stages
		// keep whatever spacing the user had after it.
		rest = " " + strings.TrimLeft(pipeline[i:], " \t")
	}
	return name + " " + strings.Join(rewrittenArgs, " ") + rest, nil
}

// containsGlobMeta reports whether s contains any of the glob
// metacharacters bash recognizes outside of `[...]` ranges. We don't
// implement [...] expansion (rarely used against pseudofiles); flag
// it the same as * for the "has glob" test so callers don't reach the
// planner with an unexpanded `[`.
func containsGlobMeta(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

// errCrossSourceGlob marks a glob that would read across services or
// namespaces. ExpandSourceGlobs turns it into the user-facing refusal.
var errCrossSourceGlob = errors.New("cross-source glob")

// matchPseudofileGlob enumerates the pseudofile paths from `apps`
// that match `pattern`. A glob matches kinds inside one service
// directory, e.g. /log/<ns>/<env>/<svc>/*.log or /log/<ns>/<svc>/*.log.
// Anything wider (a glob in a directory segment, or a directory that
// holds services rather than kinds) returns errCrossSourceGlob.
//
// Apps come from coordinator.SeenServices in the encoded form
// "<ns>/<env>/<svc>/<kind>" for env-bearing rows or "<ns>/<svc>/<kind>"
// for env-less rows (see encodeName in coordinator/coordinator.go).
// A service directory is the app name without its kind segment.
func matchPseudofileGlob(pattern string, apps []string) ([]string, error) {
	dir := path.Dir(pattern)
	base := path.Base(pattern)

	if !strings.HasPrefix(dir, "/log/") || containsGlobMeta(dir) {
		return nil, errCrossSourceGlob
	}
	rel := strings.TrimPrefix(dir, "/log/")

	isServiceDir := false
	var matches []string
	seen := map[string]bool{}
	for _, app := range apps {
		if path.Dir(app) != rel {
			continue
		}
		isServiceDir = true
		kind := path.Base(app)
		ok, err := path.Match(base, kind+".log")
		if err != nil {
			return nil, fmt.Errorf("glob: %w", err)
		}
		if !ok {
			continue
		}
		full := dir + "/" + kind + ".log"
		if !seen[full] {
			seen[full] = true
			matches = append(matches, full)
		}
	}
	if !isServiceDir {
		return nil, errCrossSourceGlob
	}
	sort.Strings(matches)
	return matches, nil
}

// indexUnquotedPipe returns the byte offset of the first `|` that
// isn't inside single or double quotes. -1 if no pipe at top level.
func indexUnquotedPipe(s string) int {
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 < len(s) {
				i++ // skip escaped char
			}
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '|':
			if !inSingle && !inDouble {
				return i
			}
		}
	}
	return -1
}
