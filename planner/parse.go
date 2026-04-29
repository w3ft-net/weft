package planner

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kevlarcon/warp/pkg/bash"
)

// sourceInfo describes the source command (cat/head/tail) at the start of a pipeline.
//
// Multi-arg `cat a.log b.log` (and the post-glob-expansion form of
// `cat *.log`) populates apps with the union; the planner emits a
// single `app IN (...)` predicate. apps is empty for warp.log
// (all apps); a one-element apps means a single-app query and uses
// `app =`/`LIKE` predicates.
type sourceInfo struct {
	apps   []string // app names; empty = warp.log (no app filter)
	env    string   // env extracted from pseudofile path; shared across all apps
	cmd    string   // "cat", "head", or "tail"
	limit  int      // for head/tail: line count (0 = unlimited for cat)
	follow bool     // tail -f
}

// app returns the single app name for queries that source a single
// pseudofile. Use only in code paths that already gate on
// len(apps) == 1.
func (s sourceInfo) app() string {
	if len(s.apps) == 0 {
		return ""
	}
	return s.apps[0]
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
func parsePipeline(pipeline string) (*pipelineInfo, error) {
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

	src, err := parseSource(srcCmd)
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

// parseSource extracts the source command info from the first pipeline command.
func parseSource(cmd *bash.SimpleCommand) (*sourceInfo, error) {
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
		apps, env, err := extractAppsEnv(args, "cat")
		if err != nil {
			return nil, err
		}
		return &sourceInfo{apps: apps, env: env, cmd: "cat"}, nil

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
		apps, env, err := extractAppsEnv(files, "head")
		if err != nil {
			return nil, err
		}
		return &sourceInfo{apps: apps, env: env, cmd: "head", limit: limit}, nil

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
		apps, env, err := extractAppsEnv(files, "tail")
		if err != nil {
			return nil, err
		}
		return &sourceInfo{apps: apps, env: env, cmd: "tail", limit: limit, follow: follow}, nil

	default:
		return nil, fmt.Errorf("unknown source command: %s", name)
	}
}

// extractAppsEnv extracts apps + a shared env from a list of
// pseudofile arguments. All args must agree on env (or all have
// empty env); mixing prod-scoped and sand-scoped paths in one cat is
// rejected because we'd silently drop one side. Empty app names are
// dropped — extractApp returns "" for warp.log (the all-apps wildcard),
// which means no app predicate at all, equivalent to a fan-out across
// every app the customer can read. Apps are deduplicated and sorted
// for stable SQL output.
func extractAppsEnv(files []string, cmd string) ([]string, string, error) {
	seen := map[string]struct{}{}
	var apps []string
	var env string
	for i, f := range files {
		a := extractApp(f)
		if a != "" {
			if _, ok := seen[a]; !ok {
				seen[a] = struct{}{}
				apps = append(apps, a)
			}
		}
		e := extractEnv(f)
		if i == 0 {
			env = e
		} else if e != env {
			return nil, "", fmt.Errorf("%s: cannot mix env=%q and env=%q in one pipeline", cmd, env, e)
		}
	}
	// Stable order so SQL is deterministic for tests and caching.
	if len(apps) > 1 {
		strSliceSort(apps)
	}
	return apps, env, nil
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

// extractApp extracts the app name from a pseudofile name.
//
// Path layout (segments after /log/):
//
//	1 seg → "<app>"                       whole-customer query (flat or namespace-prefix wildcard)
//	2 seg → "<ns>/<app>"                  app within a namespace; env (if any) supplied by the token
//	3 seg → "<ns>/<service>/<kind>"       telemetry stream: metrics/heartbeats/runtime per service
//	4 seg → "<ns>/<env>/<service>/<kind>" same as 3 seg with an explicit env filter (env-bound multi-env tokens)
//
// "warp.log" remains the all-apps wildcard. The previous 3-seg form
// "<ns>/<env>/<app>" no longer applies — env-bound tokens get the env
// predicate stamped server-side via the auth interceptor; multi-env
// tokens that need a specific env in the path use the 4-seg form.
func extractApp(filename string) string {
	filename = strings.TrimPrefix(filename, "/log/")
	filename = strings.TrimPrefix(filename, "log/")
	name := strings.TrimSuffix(filename, ".log")
	if name == "warp" {
		return ""
	}
	parts := strings.Split(name, "/")
	if len(parts) == 4 {
		return parts[0] + "/" + parts[2] + "/" + parts[3]
	}
	return name
}

// extractEnv pulls the env segment from a 4-level pseudofile path.
// "/log/weft/prod/coordinator/runtime.log" → "prod"
// "/log/weft/coordinator/runtime.log"      → ""
// See extractApp for the full layout rules.
func extractEnv(filename string) string {
	filename = strings.TrimPrefix(filename, "/log/")
	filename = strings.TrimPrefix(filename, "log/")
	name := strings.TrimSuffix(filename, ".log")
	parts := strings.Split(name, "/")
	if len(parts) == 4 {
		return parts[1]
	}
	return ""
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
