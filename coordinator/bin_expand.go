package coordinator

import (
	"context"
	"fmt"
	warpfs "github.com/graybeardcorp/warp/pkg/fs"

	"github.com/graybeardcorp/warp/pkg/bash"
)

// rejectBinInFollow returns an error if pipeline is a `tail -f` pipeline
// that references any /bin/ script (bash or Lua). We refuse uniformly
// rather than try to classify: bash bodies can invoke functions, eval,
// sourced files, or subshells that hide aggregating stages from the
// planner's follow-mode gate; Lua bodies are opaque to classifyStage
// entirely. A single "no /bin/ in live mode" rule keeps the semantics
// honest and the error message short.
//
// The check is a cheap pre-pass: one bash.Parse, one FS.ReadFile per
// stage command. It's called from coordinator.query *before* the /bin/
// bash body expansion, so we fail out before any inlining happens. If
// fs is nil (no customer context resolved) the check no-ops — there
// are no /bin/ scripts to clash with.
func rejectBinInFollow(ctx context.Context, pipeline string, fs warpfs.FS) error {
	if fs == nil {
		return nil
	}
	ast, err := bash.Parse(pipeline)
	if err != nil {
		// A parse error surfaces with a better message from the planner
		// a few steps downstream. Not our job to reject here.
		return nil
	}
	if len(ast.Commands) == 0 {
		return nil
	}
	list := ast.Commands[0]
	if list == nil || list.First == nil || len(list.First.Commands) == 0 {
		return nil
	}

	leading, ok := list.First.Commands[0].(*bash.SimpleCommand)
	if !ok || !isTailFollow(leading) {
		return nil
	}

	// Walk every stage after the tail -f source; the source itself is
	// `tail`, not a /bin/ script.
	for _, cmd := range list.First.Commands[1:] {
		sc, ok := cmd.(*bash.SimpleCommand)
		if !ok || len(sc.Words) == 0 {
			continue
		}
		name := sc.Words[0].Literal()
		if name == "" {
			continue
		}
		if _, err := fs.ReadFile(ctx, "/bin/"+name); err == nil {
			return fmt.Errorf(
				"plan: `/bin/%s` can't run under `tail -f` — /bin/ scripts "+
					"(bash or lua) may contain aggregating stages or opaque "+
					"logic that doesn't fit a live-streaming pipeline. Invoke "+
					"it from a cron, or inline the specific commands "+
					"you need into the follow pipeline directly",
				name)
		}
	}
	return nil
}

// isTailFollow returns true when cmd is `tail -f ...` — the source
// shape that makes the whole pipeline live. Any `-f` anywhere in the
// argv counts, matching the planner's own detection in parse.go.
func isTailFollow(cmd *bash.SimpleCommand) bool {
	if len(cmd.Words) == 0 {
		return false
	}
	if cmd.Words[0].Literal() != "tail" {
		return false
	}
	for _, w := range cmd.Words[1:] {
		if w.Literal() == "-f" {
			return true
		}
	}
	return false
}

// expandBinPipeline inspects the leading command of pipeline. If it's an
// invocation of a bash /bin script stored in fs, the script's body is
// parsed, positional args are bound into the AST, and the body's commands
// are spliced into the pipeline in place of the original invocation.
//
// The result is that a user-facing invocation like
//
//	count_errors disk.log | wc -l
//
// arrives at planner.Plan as
//
//	cat /log/disk.log | grep ERROR | wc -l
//
// — letting the planner recognize `cat /log/...` as a source command
// and push it into shard SQL. If pipeline doesn't start with a /bin bash
// invocation (or if the shape is too complex to splice safely), it's
// returned unchanged; warp will resolve /bin at execution time instead.
//
// Lua /bin scripts are intentionally skipped — they execute through
// warp's Lua runtime at runLocal time, not as textual pipelines.
func expandBinPipeline(ctx context.Context, pipeline string, fs warpfs.FS) (string, error) {
	if fs == nil {
		return pipeline, nil
	}
	ast, err := bash.Parse(pipeline)
	if err != nil {
		// Let the caller / planner surface the parse error with its own
		// context. We don't want to swallow it silently.
		return pipeline, nil
	}
	if len(ast.Commands) == 0 {
		return pipeline, nil
	}
	list := ast.Commands[0]
	if list == nil || list.First == nil || len(list.First.Commands) == 0 {
		return pipeline, nil
	}
	leading, ok := list.First.Commands[0].(*bash.SimpleCommand)
	if !ok || len(leading.Words) == 0 {
		return pipeline, nil
	}

	name := leading.Words[0].Literal()
	if name == "" {
		return pipeline, nil
	}
	path := "/bin/" + name

	data, err := fs.ReadFile(ctx, path)
	if err != nil {
		return pipeline, nil // not a /bin script
	}

	// Skip Lua — warp runs those through luapkg.RunScript at exec time.
	if lfs, ok := fs.(warpfs.LangFS); ok && lfs.FileLanguage(ctx, path) == "lua" {
		return pipeline, nil
	}

	bodyAST, err := bash.Parse(string(data))
	if err != nil {
		return "", fmt.Errorf("/bin/%s: parse body: %w", name, err)
	}
	if len(bodyAST.Commands) != 1 || len(bodyAST.Commands[0].Rest) != 0 {
		// Multi-list bodies (foo && bar; baz || qux) can't be spliced
		// into a pipeline cleanly. Fall back to runtime resolution.
		return pipeline, nil
	}
	bodyList := bodyAST.Commands[0]
	if bodyList.First == nil {
		return pipeline, nil
	}
	bodyCmds := bodyList.First.Commands
	if len(bodyCmds) == 0 {
		return pipeline, nil
	}
	// Splicing is only valid when every stage of the body pipeline is
	// a SimpleCommand. A body like `for x in a b; do ...; done` parses
	// as a single pipeline whose sole command is a ForClause — not a
	// shape we can splice into an outer pipeline (nor would it make
	// semantic sense). Let runtime resolution handle those cases.
	for _, c := range bodyCmds {
		if _, ok := c.(*bash.SimpleCommand); !ok {
			return pipeline, nil
		}
	}

	// Collect positional args from the invocation (all words after the
	// command name). Only literal, fully-resolved args are supported —
	// if one of them is itself an expansion we don't evaluate it here.
	args := make([]string, 0, len(leading.Words)-1)
	for _, w := range leading.Words[1:] {
		args = append(args, w.Literal())
	}

	// Bind positionals inside the body.
	bash.BindPositionals(bodyAST, args)

	// Splice the body's commands into the outer pipeline, replacing the
	// original /bin invocation.
	list.First.Commands = append(bodyCmds, list.First.Commands[1:]...)

	return bash.Unparse(ast), nil
}
