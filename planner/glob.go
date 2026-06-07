package planner

import (
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
// Globs operate on the *file segment* of the path: `/log/<ns>/<env>/`
// (or `/log/<ns>/`) is preserved as the directory prefix; the trailing
// `*.log` (or similar) is matched against the available apps' file
// names within that directory. So `cat /log/acme/prod/*.log` with apps
// {"acme/api", "acme/worker"} becomes
// `cat /log/acme/prod/api.log /log/acme/prod/worker.log`.
//
// Glob with no matches is an error (mirroring bash's `failglob`
// behavior — silently dropping it would surface as an empty result
// the user couldn't distinguish from "no rows" and is the wrong
// failure mode for an analyst's interactive shell).
func ExpandSourceGlobs(pipeline string, apps []string) (string, error) {
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
		matches, err := matchPseudofileGlob(arg, apps)
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

// matchPseudofileGlob enumerates the pseudofile paths from `apps`
// that match `pattern`.
//
// Pattern shapes:
//   - /log/<ns>/<env>/<glob>.log — globs apps within (ns, env).
//   - /log/<ns>/<glob>.log       — globs apps directly under <ns>.
//
// Apps come from coordinator.SeenServices in the encoded form
// "<ns>/<env>/<deployment>" for env-bearing rows or "<ns>/<deployment>"
// for env-less rows (see encodeName in coordinator/coordinator.go).
// The matcher treats the full dir-after-/log/ as a prefix and strips
// it from candidate apps; the remainder is what gets matched against
// the glob's basename.
func matchPseudofileGlob(pattern string, apps []string) ([]string, error) {
	dir := path.Dir(pattern)
	base := path.Base(pattern)

	if !strings.HasPrefix(dir, "/log") {
		return nil, fmt.Errorf("glob: unsupported directory %q (only /log paths support globs)", dir)
	}
	rel := strings.TrimPrefix(dir, "/log")
	rel = strings.TrimPrefix(rel, "/")
	prefix := ""
	if rel != "" {
		prefix = rel + "/"
	}

	var matches []string
	seen := map[string]bool{}
	for _, app := range apps {
		if prefix != "" && !strings.HasPrefix(app, prefix) {
			continue
		}
		rest := strings.TrimPrefix(app, prefix)
		if strings.Contains(rest, "/") {
			continue // nested deeper than the glob's directory
		}
		ok, err := path.Match(base, rest+".log")
		if err != nil {
			return nil, fmt.Errorf("glob: %w", err)
		}
		if !ok {
			continue
		}
		full := dir + "/" + rest + ".log"
		if !seen[full] {
			seen[full] = true
			matches = append(matches, full)
		}
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
