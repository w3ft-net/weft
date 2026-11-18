package coordinator

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/graybeardcorp/warp/pkg/bash"
)

// RegisterWeftCommands registers weft::latest, weft::previous, weft::describe, weft::avg, weft::max,
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
	// latest and previous are point reads: one cron name, no window.
	for name, read := range map[string]func(string) (string, error){
		"weft::latest":   wf.Latest,
		"weft::previous": wf.Previous,
	} {
		name, read := name, read
		regSpec(interp, bash.Spec{
			Name: name, Summary: "print the cron's " + strings.TrimPrefix(name, "weft::") + " value",
			Args: cronOps(1, 1, "CRON"),
		}, func(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
			val, err := read(args[0])
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			fmt.Fprintln(stdout, val)
			return 0
		})
	}

	// describe prints how past runs went, one JSON object per line, newest
	// first: the saved fields no other weft:: function exposes.
	regSpec(interp, bash.Spec{
		Name: "weft::describe", Summary: "print how a cron's recent runs went, as JSON lines",
		Args:    cronOps(1, 2, "CRON", "N"),
		TypeDoc: "weft::describe <name> [n]: the last n runs (default 10, max 50) as JSON lines: ran_at, exit_code, duration_ms, tag, value, stdout, stderr",
	}, func(args []string, _ io.Reader, stdout io.Writer, stderr io.Writer) int {
		n := defaultDescribeRuns
		if len(args) == 2 {
			v, err := strconv.Atoi(args[1])
			if err != nil {
				fmt.Fprintf(stderr, "weft::describe: invalid n %q\n", args[1])
				return 1
			}
			n = v
		}
		runs, err := wf.Describe(args[0], n)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := writeRunsJSONL(stdout, runs); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	})

	register := func(name, typeDoc string, call func(name, dur string) (float64, error)) {
		regSpec(interp, bash.Spec{
			Name: name, Summary: "compute " + strings.TrimPrefix(name, "weft::") + " of a cron's results over a window",
			Args:    cronOps(2, 2, "CRON", "DURATION"),
			TypeDoc: typeDoc,
		}, func(args []string, _ io.Reader, stdout io.Writer, stderr io.Writer) int {
			val, err := call(args[0], args[1])
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			fmt.Fprintf(stdout, "%.6g\n", val)
			return 0
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
			d, err := ParseRawDuration(dur)
			if err != nil {
				return 0, err
			}
			return wf.First(name, d)
		})
	register("weft::count",
		"weft::count <name> <dur>: number of times the cron ran in the window",
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
			d, err := ParseRawDuration(dur)
			if err != nil {
				return 0, err
			}
			return wf.Delta(name, d)
		})
	register("weft::slope",
		"weft::slope is equivalent to:\n    $(weft::delta <name> <dur>) / <dur-in-seconds>",
		func(name, dur string) (float64, error) {
			d, err := ParseRawDuration(dur)
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
	regSpec(interp, bash.Spec{
		Name: "weft::percentile", Summary: "compute a percentile of a cron's results over a window",
		Args:    cronOps(3, 3, "CRON", "DURATION", "P"),
		TypeDoc: "weft::percentile <name> <dur> <p>: linearly interpolated p-th percentile of numeric stdout in window (p in 0..100)",
	}, func(args []string, _ io.Reader, stdout io.Writer, stderr io.Writer) int {
		d, err := ParseRawDuration(args[1])
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
	})
}
