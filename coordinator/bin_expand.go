package coordinator

import (
	"fmt"

	"github.com/kevlarcon/warp/pkg/bash"
)

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
func expandBinPipeline(pipeline string, fs bash.FS) (string, error) {
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

	data, err := fs.ReadFile(path)
	if err != nil {
		return pipeline, nil // not a /bin script
	}

	// Skip Lua — warp runs those through luapkg.RunScript at exec time.
	if lfs, ok := fs.(bash.LangFS); ok && lfs.FileLanguage(path) == "lua" {
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
