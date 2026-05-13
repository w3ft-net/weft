package coordinator

import (
	"fmt"
	"io"
	"strconv"

	"github.com/kevlarcon/warp/pkg/bash"
)

// RegisterWeftCommands registers weft::latest, weft::avg, weft::max,
// weft::min, weft::sum, weft::stddev, weft::first, weft::count,
// weft::percentile, weft::delta, weft::slope, weft::rate as bash
// commands on the interpreter. Composite funcs (delta, slope, rate)
// register a TypeDoc that shows their equivalent in terms of the
// primitive funcs, so `type weft::delta` teaches the recipe even
// though the implementation is native.
//
// Bash uses :: as the namespace separator; Lua reaches the same set
// via the `weft.*` table (`weft.latest(...)` etc.).
func RegisterWeftCommands(interp *bash.Interpreter, wf *WeftFuncs) {
	interp.RegisterCommand("weft::latest", func(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
		if len(args) < 1 {
			fmt.Fprintln(stderr, "usage: weft::latest <watch-name>")
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

	register := func(name, typeDoc string, call func(name, dur string) (float64, error)) {
		interp.RegisterCommandMeta(name, bash.ExternalCmd{
			Strategy: bash.StageBarrier,
			TypeDoc:  typeDoc,
			Run: func(args []string, _ io.Reader, stdout io.Writer, stderr io.Writer) int {
				if len(args) < 2 {
					fmt.Fprintf(stderr, "usage: %s <watch-name> <duration>\n", name)
					return 1
				}
				val, err := call(args[0], args[1])
				if err != nil {
					fmt.Fprintln(stderr, err)
					return 1
				}
				fmt.Fprintf(stdout, "%.6g\n", val)
				return 0
			},
		})
	}

	register("weft::avg", "", func(name, dur string) (float64, error) {
		d, err := ParseAggDuration(dur)
		if err != nil {
			return 0, err
		}
		return wf.Avg(name, d)
	})
	register("weft::max", "", func(name, dur string) (float64, error) {
		d, err := ParseAggDuration(dur)
		if err != nil {
			return 0, err
		}
		return wf.Max(name, d)
	})
	register("weft::min", "", func(name, dur string) (float64, error) {
		d, err := ParseAggDuration(dur)
		if err != nil {
			return 0, err
		}
		return wf.Min(name, d)
	})
	register("weft::sum", "", func(name, dur string) (float64, error) {
		d, err := ParseAggDuration(dur)
		if err != nil {
			return 0, err
		}
		return wf.Sum(name, d)
	})
	register("weft::stddev", "", func(name, dur string) (float64, error) {
		d, err := ParseAggDuration(dur)
		if err != nil {
			return 0, err
		}
		return wf.Stddev(name, d)
	})
	register("weft::first",
		"weft::first <name> <dur>: oldest numeric stdout still in the window (anchor for delta/slope; cap 14d)",
		func(name, dur string) (float64, error) {
			d, err := ParseEndpointDuration(dur)
			if err != nil {
				return 0, err
			}
			return wf.First(name, d)
		})
	register("weft::count",
		"weft::count <name> <dur>: number of times the watch ran in the window",
		func(name, dur string) (float64, error) {
			d, err := ParseAggDuration(dur)
			if err != nil {
				return 0, err
			}
			return wf.Count(name, d)
		})
	register("weft::delta",
		"weft::delta is equivalent to:\n    $(weft::latest <name>) - $(weft::first <name> <dur>)",
		func(name, dur string) (float64, error) {
			d, err := ParseEndpointDuration(dur)
			if err != nil {
				return 0, err
			}
			return wf.Delta(name, d)
		})
	register("weft::slope",
		"weft::slope is equivalent to:\n    $(weft::delta <name> <dur>) / <dur-in-seconds>",
		func(name, dur string) (float64, error) {
			d, err := ParseEndpointDuration(dur)
			if err != nil {
				return 0, err
			}
			return wf.Slope(name, d)
		})
	register("weft::rate",
		"weft::rate is equivalent to:\n    $(weft::sum <name> <dur>) / <dur-in-seconds>",
		func(name, dur string) (float64, error) {
			d, err := ParseAggDuration(dur)
			if err != nil {
				return 0, err
			}
			return wf.Rate(name, d)
		})

	// weft::percentile takes an extra p argument (0..100).
	interp.RegisterCommandMeta("weft::percentile", bash.ExternalCmd{
		Strategy: bash.StageBarrier,
		TypeDoc:  "weft::percentile <name> <dur> <p>: linearly interpolated p-th percentile of numeric stdout in window (p in 0..100)",
		Run: func(args []string, _ io.Reader, stdout io.Writer, stderr io.Writer) int {
			if len(args) < 3 {
				fmt.Fprintln(stderr, "usage: weft::percentile <watch-name> <duration> <p>")
				return 1
			}
			d, err := ParseAggDuration(args[1])
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			p, err := strconv.ParseFloat(args[2], 64)
			if err != nil {
				fmt.Fprintf(stderr, "weft::percentile: invalid p %q\n", args[2])
				return 1
			}
			val, err := wf.Percentile(args[0], d, p)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			fmt.Fprintf(stdout, "%.6g\n", val)
			return 0
		},
	})

}
