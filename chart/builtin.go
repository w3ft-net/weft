package chart

import (
	"github.com/graybeardcorp/warp/pkg/shellcore"
)

// Builtin returns a shellcore.BuiltinFunc that handles `chart …`
// typed at the prompt (the watch form, not the pipe-stage form).
// One-shot renders go straight to scrollback; --follow enters the
// chart View for live redraw.
//
// Wire it via core.RegisterBuiltin("chart", chart.Builtin(wr)).
func Builtin(wr WatchResultsProvider) shellcore.BuiltinFunc {
	return func(c *shellcore.ShellCore, parts []string) bool {
		if len(parts) == 0 || parts[0] != "chart" {
			return false
		}
		args, errMsg := ParseWatchCmd(parts[1:])
		if errMsg != "" {
			c.AppendScrollback(shellcore.ErrorText(errMsg) + "\n")
			return true
		}

		if args.Follow {
			if err := c.EnterView("chart", args); err != nil {
				c.AppendScrollback(shellcore.ErrorText("chart: "+err.Error()) + "\n")
			}
			return true
		}

		w, h := c.Width(), c.Height()-1
		if w < 20 {
			w = 20
		}
		if h < 6 {
			h = 6
		}
		out, msg := RenderOneShot(wr, args.Name, args.Kind, args.Limit, w, h)
		if msg != "" {
			c.AppendScrollback(shellcore.ErrorText("chart: "+msg) + "\n")
			return true
		}
		c.AppendScrollback(out)
		return true
	}
}

// RenderPipe is the shellcore.PipeStageRenderFunc for `... | chart`.
// Receives the upstream output lines + the parsed PipeArgs and
// writes the rendered chart to scrollback.
//
// Wire it via:
//
//	core.RegisterPipeStage("chart", chart.ParsePipeArgs, chart.RenderPipe)
func RenderPipe(c *shellcore.ShellCore, lines []string, parsed any) (string, string) {
	args, ok := parsed.(PipeArgs)
	if !ok {
		return "", "chart: internal error: parsed args not PipeArgs"
	}
	w, h := c.Width(), c.Height()-1
	if w < 20 {
		w = 20
	}
	if h < 6 {
		h = 6
	}
	return RenderFromStream(lines, args, w, h)
}

// Register wires chart into the given shellcore at construction
// time. Convenience wrapper that handles all three registration
// hooks (View + Builtin + PipeStage) in one call.
//
// Embedders that want fine-grained control over each hook can
// register them individually instead.
func Register(c *shellcore.ShellCore, wr WatchResultsProvider) {
	c.RegisterView(NewView(wr))
	c.RegisterBuiltin("chart", Builtin(wr))
	c.RegisterPipeStage("chart", ParsePipeArgs, RenderPipe)
}
