package planner

import (
	"strings"

	"github.com/graybeardcorp/warp/pkg/bash"
)

// SplitAlertStage separates a trailing "| alert ..." stage from a pipeline.
// When the last pipe stage is `alert`, it returns the pipeline with that
// stage removed plus the alert's argv (the tokens *after* the "alert"
// command name — matching what alert.ParseArgs expects). When
// the pipeline is just `alert ...`, queryPart is empty. When there is
// no terminal alert stage, returns (pipeline, nil).
//
// Alert is only recognised as the terminal stage of the top-level
// pipeline. Alerts buried in compound commands or command substitutions
// are rejected earlier by store.CompileWatchPipeline and are not the
// runtime's problem.
//
// The AST is consulted to identify the alert stage and extract its
// quoted-aware argv. The upstream query half is sliced from the
// original pipeline string (not reconstructed from AST literals) so
// any quoting inside awk/jq/sed stages is preserved byte-for-byte.
func SplitAlertStage(pipeline string) (queryPart string, alertArgs []string) {
	script, err := bash.Parse(pipeline)
	if err != nil {
		return pipeline, nil
	}
	if len(script.Commands) == 0 || script.Commands[0].First == nil {
		return pipeline, nil
	}
	cmds := script.Commands[0].First.Commands
	if len(cmds) == 0 {
		return pipeline, nil
	}
	last, ok := cmds[len(cmds)-1].(*bash.SimpleCommand)
	if !ok || len(last.Words) == 0 || last.Words[0].Literal() != "alert" {
		return pipeline, nil
	}
	alertArgs = make([]string, 0, len(last.Words)-1)
	for _, w := range last.Words[1:] {
		alertArgs = append(alertArgs, w.Literal())
	}
	if len(cmds) == 1 {
		return "", alertArgs
	}
	idx := strings.LastIndex(pipeline, "| alert")
	if idx == -1 {
		// AST says there's a terminal alert but we can't find it in the
		// source — bail out rather than risk a corrupted query half.
		return pipeline, nil
	}
	return strings.TrimSpace(pipeline[:idx]), alertArgs
}
