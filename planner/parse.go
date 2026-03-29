package planner

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kevlarcon/warp/pkg/bash"
)

// sourceInfo describes the source command (cat/head/tail) at the start of a pipeline.
type sourceInfo struct {
	app   string // app name extracted from pseudofile, or "" for warp.log (all apps)
	cmd   string // "cat", "head", or "tail"
	limit int    // for head/tail: line count (0 = unlimited for cat)
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
		return &sourceInfo{app: app, cmd: "cat"}, nil

	case "head":
		limit := 10
		file := ""
		for _, arg := range args {
			if strings.HasPrefix(arg, "-") {
				n, err := strconv.Atoi(arg[1:])
				if err == nil {
					limit = n
				}
			} else {
				file = arg
			}
		}
		if file == "" {
			return nil, fmt.Errorf("head: missing pseudofile argument")
		}
		app := extractApp(file)
		return &sourceInfo{app: app, cmd: "head", limit: limit}, nil

	case "tail":
		limit := 10
		file := ""
		follow := false
		for _, arg := range args {
			if arg == "-f" {
				follow = true
			} else if strings.HasPrefix(arg, "-") {
				n, err := strconv.Atoi(arg[1:])
				if err == nil {
					limit = n
				}
			} else {
				file = arg
			}
		}
		_ = follow // tail -f handled separately in future
		if file == "" {
			return nil, fmt.Errorf("tail: missing pseudofile argument")
		}
		app := extractApp(file)
		return &sourceInfo{app: app, cmd: "tail", limit: limit}, nil

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
// "vimgym.log" → "vimgym", "warp.log" → "" (all apps).
func extractApp(filename string) string {
	name := strings.TrimSuffix(filename, ".log")
	if name == "warp" {
		return ""
	}
	return name
}

// literalArgs extracts literal strings from a slice of Words.
func literalArgs(words []bash.Word) []string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = w.Literal()
	}
	return out
}
