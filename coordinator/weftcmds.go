package coordinator

import (
	"fmt"
	"io"

	"github.com/kevlarcon/warp/pkg/bash"
)

// RegisterWeftCommands registers weft.latest, weft.avg, weft.max, weft.min,
// weft.sum, weft.stddev as bash commands on the interpreter.
func RegisterWeftCommands(interp *bash.Interpreter, wf *WeftFuncs) {
	interp.RegisterCommand("weft.latest", func(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
		if len(args) < 1 {
			fmt.Fprintln(stderr, "usage: weft.latest <watch-name>")
			return 1
		}
		val, err := wf.Latest(args[0])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, val)
		return 0
	})

	for _, fn := range []struct {
		name string
		call func(string, string) (float64, error)
	}{
		{"weft.avg", func(name, dur string) (float64, error) {
			d, err := ParseAggDuration(dur)
			if err != nil {
				return 0, err
			}
			return wf.Avg(name, d)
		}},
		{"weft.max", func(name, dur string) (float64, error) {
			d, err := ParseAggDuration(dur)
			if err != nil {
				return 0, err
			}
			return wf.Max(name, d)
		}},
		{"weft.min", func(name, dur string) (float64, error) {
			d, err := ParseAggDuration(dur)
			if err != nil {
				return 0, err
			}
			return wf.Min(name, d)
		}},
		{"weft.sum", func(name, dur string) (float64, error) {
			d, err := ParseAggDuration(dur)
			if err != nil {
				return 0, err
			}
			return wf.Sum(name, d)
		}},
		{"weft.stddev", func(name, dur string) (float64, error) {
			d, err := ParseAggDuration(dur)
			if err != nil {
				return 0, err
			}
			return wf.Stddev(name, d)
		}},
	} {
		fn := fn // capture
		interp.RegisterCommand(fn.name, func(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
			if len(args) < 2 {
				fmt.Fprintf(stderr, "usage: %s <watch-name> <duration>\n", fn.name)
				return 1
			}
			val, err := fn.call(args[0], args[1])
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			fmt.Fprintf(stdout, "%.6g\n", val)
			return 0
		})
	}
}
