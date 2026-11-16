package witrepo

import (
	"fmt"
	"io"
	"strings"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/warp/pkg/gitcore"
	"github.com/graybeardcorp/warp/pkg/syntax"
)

// commandUsage is what bare `git` and `git --help` print.
const commandUsage = "usage: git <command> [<args>]\n\nweft supports a subset of git over /bin, /cron.d and /etc:\n  log show diff status blame rev-parse\n  restore checkout -- <path>  revert  commit --amend"

// RegisterCommand registers `git` as a bash command: a strict subset of
// git over the group's /bin, /cron.d and /etc history (see New). Anything git has
// that the subset lacks is refused by name, so a missing feature reads as
// ours and a typo reads as the user's.
//
// The interpreter's stdout decides colour: output going straight to the user
// is coloured (--color=always|never overrides); output headed into a pipe is
// plain text.
func RegisterCommand(interp *bash.Interpreter, cfg Config) {
	interp.RegisterCommand("git", func(args []string, _ io.Reader, stdout, stderr io.Writer) int {
		if len(args) == 0 {
			fmt.Fprintln(stderr, commandUsage)
			return 1
		}
		// The unknown-command error tells people to run `git --help`.
		if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
			fmt.Fprintln(stdout, commandUsage)
			return 0
		}
		env := gitcore.Env{
			Product:    "weft",
			IsTerminal: func() bool { return interp.IsUserStdout(stdout) },
			Colorize:   colorDiff,
		}
		out, err := gitcore.Run(New(cfg), env, args[0], args[1:])
		if err != nil {
			fmt.Fprintln(stderr, err)
			if strings.HasPrefix(err.Error(), "fatal:") {
				return 128
			}
			return 1
		}
		if out != "" {
			if !strings.HasSuffix(out, "\n") {
				out += "\n"
			}
			fmt.Fprint(stdout, out)
		}
		return 0
	})
}

// colorDiff highlights diff and log text. The core emits plain text; colour
// is added only when the output goes straight to the terminal.
func colorDiff(s string) string {
	if s == "" {
		return ""
	}
	return syntax.Render(syntax.LangDiff, s)
}
