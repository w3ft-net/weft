package coordinator

import (
	"fmt"
	"io"
	"strings"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/warp/pkg/gitcore"
	"github.com/graybeardcorp/warp/pkg/syntax"

	"github.com/graybeardcorp/weft/witrepo"
)

// RegisterGitCommand registers `git` as a bash command: a strict subset of
// git over the org's /bin and /cron.d history (see witrepo). Anything git has
// that the subset lacks is refused by name, so a missing feature reads as
// ours and a typo reads as the user's.
//
// The interpreter's stdout decides colour: output going straight to the user
// is coloured (--color=always|never overrides); output headed into a pipe is
// plain text.
func RegisterGitCommand(interp *bash.Interpreter, cfg witrepo.Config) {
	interp.RegisterCommand("git", func(args []string, _ io.Reader, stdout, stderr io.Writer) int {
		if len(args) == 0 {
			fmt.Fprintln(stderr, "usage: git <command> [<args>]\n\nweft supports a subset of git over /bin and /cron.d:\n  log show diff status restore checkout revert commit rev-parse blame")
			return 1
		}
		env := gitcore.Env{
			Product:    "weft",
			IsTerminal: func() bool { return interp.IsUserStdout(stdout) },
			Colorize:   colorGit,
		}
		out, err := gitcore.Run(witrepo.New(cfg), env, args[0], args[1:])
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

// colorGit highlights diff and log text. The core emits plain text; colour
// is added only when the output goes straight to the terminal.
func colorGit(s string) string {
	if s == "" {
		return ""
	}
	return syntax.Render(syntax.LangDiff, s)
}
