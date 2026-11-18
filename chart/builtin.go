package chart

import (
	"fmt"
	"io"
	"strings"

	"github.com/graybeardcorp/warp/pkg/shellcore"
)

// Command returns the `chart` command. One name, two forms, told apart by
// whether anything is piped into it:
//
//   - alone, it charts a cron (`chart <name> ...`): a one-shot render goes
//     straight to the terminal, --follow enters the chart View for live
//     redraw;
//   - after a pipe, it charts the upstream output (`... | chart -x 1`),
//     spanning the shell's range.
//
// rangeSpec returns the shell's current range spec; it is read on every
// render, so `range` changes and relative ranges slide. Anything that needs
// the core (the terminal size, entering the View) goes through OnLoop.
//
// Wire it via Register, or core.RegisterCommand("chart", chart.Command(src,
// rangeSpec)) with core.RegisterView(chart.NewView(src, rangeSpec)).
func Command(src CronSource, rangeSpec func() string) func(cx *shellcore.Cmd) int {
	return func(cx *shellcore.Cmd) int {
		fail := func(msg string) int {
			fmt.Fprintln(cx.Stderr, shellcore.ErrorText(msg))
			return 1
		}
		size := func() (w, h int) {
			cx.OnLoop(func(c *shellcore.ShellCore) error {
				w, h = c.Width(), c.Height()-1
				return nil
			})
			if w < 20 {
				w = 20
			}
			if h < 6 {
				h = 6
			}
			return w, h
		}

		if !cx.StdinIsTerminal() {
			parsed, errMsg := ParsePipeArgs(cx.Args)
			if errMsg != "" {
				return fail("chart: " + errMsg)
			}
			data, _ := io.ReadAll(cx.Stdin)
			var lines []string
			for _, ln := range strings.Split(string(data), "\n") {
				if strings.TrimSpace(ln) != "" {
					lines = append(lines, ln)
				}
			}
			w, h := size()
			out, msg := RenderFromStream(lines, parsed.(PipeArgs).WithRange(rangeSpec()), w, h)
			if msg != "" {
				return fail(msg)
			}
			io.WriteString(cx.Stdout, out)
			return 0
		}

		args, errMsg := ParseWatchCmd(cx.Args)
		if errMsg != "" {
			return fail(errMsg)
		}
		if args.Follow {
			if msg := CheckFollow(rangeSpec()); msg != "" {
				return fail("chart: " + msg)
			}
			if err := cx.OnLoop(func(c *shellcore.ShellCore) error { return c.EnterView("chart", args) }); err != nil {
				return fail("chart: " + err.Error())
			}
			return 0
		}
		w, h := size()
		out, msg := RenderOneShot(src, rangeSpec(), args, w, h)
		if msg != "" {
			return fail("chart: " + msg)
		}
		io.WriteString(cx.Stdout, out)
		return 0
	}
}

// Register wires chart into the given shellcore at construction time: the View
// and the `chart` command, which covers both the watch form and the pipe form.
// Embedders that want fine-grained control can register them individually.
func Register(c *shellcore.ShellCore, src CronSource, rangeSpec func() string) {
	c.RegisterView(NewView(src, rangeSpec))
	c.RegisterCommand("chart", Command(src, rangeSpec))
}
