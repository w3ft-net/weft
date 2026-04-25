package planner

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kevlarcon/warp/pkg/bash"
)

// sourceInfo describes the source command (cat/head/tail) at the start of a pipeline.
type sourceInfo struct {
	app    string // app name extracted from pseudofile, or "" for warp.log (all apps)
	env    string // env extracted from pseudofile path /log/<ns>/<env>/<app>.log; "" = no env filter
	cmd    string // "cat", "head", or "tail"
	limit  int    // for head/tail: line count (0 = unlimited for cat)
	follow bool   // tail -f
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
		app := extractApp(args[0])
		env := extractEnv(args[0])
		return &sourceInfo{app: app, env: env, cmd: "cat"}, nil

	case "head":
		limit := 10
		file := ""
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
				file = args[i]
			}
		}
		if file == "" {
			return nil, fmt.Errorf("head: missing pseudofile argument")
		}
		app := extractApp(file)
		env := extractEnv(file)
		return &sourceInfo{app: app, env: env, cmd: "head", limit: limit}, nil

	case "tail":
		limit := 10
		file := ""
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
				file = args[i]
			}
		}
		if file == "" {
			return nil, fmt.Errorf("tail: missing pseudofile argument")
		}
		app := extractApp(file)
		env := extractEnv(file)
		return &sourceInfo{app: app, env: env, cmd: "tail", limit: limit, follow: follow}, nil

	default:
		return nil, fmt.Errorf("unknown source command: %s", name)
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
// "/log/weft/coordinator.log" → "weft/coordinator"
// "/log/vimgym.log" → "vimgym" (flat namespace, backward compat)
// "warp.log" → "" (all apps)
//
// Three-level paths (/log/<ns>/<env>/<app>.log) are also accepted —
// the env segment is consumed by extractEnv, and this function returns
// just <ns>/<app> as the app filter. See docs/plans/ingest-dimensions.md
// for the path-layout rationale.
func extractApp(filename string) string {
	// Strip /log/ or log/ prefix if present.
	filename = strings.TrimPrefix(filename, "/log/")
	filename = strings.TrimPrefix(filename, "log/")
	name := strings.TrimSuffix(filename, ".log")
	if name == "warp" {
		return ""
	}
	parts := strings.Split(name, "/")
	if len(parts) == 3 {
		// /log/<ns>/<env>/<app>.log → app filter is <ns>/<app>
		return parts[0] + "/" + parts[2]
	}
	return name
}

// extractEnv pulls the env segment from a 3-level pseudofile path.
// "/log/weft/prod/coordinator.log" → "prod"
// "/log/weft/coordinator.log" → "" (no env filter — token wasn't env-bearing)
// See docs/plans/ingest-dimensions.md.
func extractEnv(filename string) string {
	filename = strings.TrimPrefix(filename, "/log/")
	filename = strings.TrimPrefix(filename, "log/")
	name := strings.TrimSuffix(filename, ".log")
	parts := strings.Split(name, "/")
	if len(parts) == 3 {
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
