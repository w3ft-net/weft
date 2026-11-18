package coordinator

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
)

// RegisterWeftPredicates registers boolean predicates over watches as
// bash builtins. Each command exits 0 (true) or 1 (false) so they
// compose with bash's native &&, ||, !:
//
//	weft::is_above error-rate 50 && weft::is_absent canary-ping && echo 1 || echo 0
//
// The predicates mirror the alert command's vocabulary so users learn
// the surface once. Same set is exposed in Lua via warp's sandbox
// under `weft.is_above(...)` / `weft.has_changed(...)` etc. — Lua uses
// the table-access dot, bash uses :: as the namespace separator.
func RegisterWeftPredicates(interp *bash.Interpreter, wf *WeftFuncs) {
	pred := func(name, summary, typeDoc string, args bash.OperandSpec, flags []bash.Flag, run weftRun) {
		regSpec(interp, bash.Spec{Name: name, Summary: summary, Flags: flags, Args: args, TypeDoc: typeDoc, UsageExit: 2}, run)
	}
	average := []bash.Flag{bash.Bool("average", 0, "with a window, compare the window's average instead of every sample")}
	direction := []bash.Flag{
		bash.Bool("above", 0, "only count a deviation above the average"),
		bash.Bool("below", 0, "only count a deviation below the average"),
	}
	pred("weft::is_above", "exit 0 if the cron's value is above a threshold",
		"weft::is_above <cron> <threshold> [<window> [--average]]: exit 0 if the cron's value is above the threshold",
		cronOps(2, 3, "CRON", "THRESHOLD", "WINDOW"), average,
		numericPredicate(wf, "weft::is_above", predicateAbove(wf)))
	pred("weft::is_below", "exit 0 if the cron's value is below a threshold",
		"weft::is_below <cron> <threshold> [<window> [--average]]: exit 0 if the cron's value is below the threshold",
		cronOps(2, 3, "CRON", "THRESHOLD", "WINDOW"), average,
		numericPredicate(wf, "weft::is_below", predicateBelow(wf)))
	pred("weft::is_absent", "exit 0 if the cron has no recent result",
		"weft::is_absent <cron> [<window>]: exit 0 if the cron has no recent result (within <window> if given, else latest is empty)",
		cronOps(1, 2, "CRON", "WINDOW"), nil, predicateAbsent(wf))
	pred("weft::has_changed", "exit 0 if the cron's latest output differs from the previous tick",
		"weft::has_changed <cron>: exit 0 if the cron's latest output differs from the previous tick",
		cronOps(1, 1, "CRON"), nil, predicateChanged(wf))
	pred("weft::is_rising", "exit 0 if the cron rose by at least a percentage",
		"weft::is_rising <cron> [<pct>] [<window>]: exit 0 if the cron rose by at least pct% (default 0) vs baseline (prev tick by default; avg over window if given)",
		cronOps(1, 3, "CRON", "PCT", "WINDOW"), nil,
		numericPredicate(wf, "weft::is_rising", predicateRising(wf)))
	pred("weft::is_falling", "exit 0 if the cron fell by at least a percentage",
		"weft::is_falling <cron> [<pct>] [<window>]: exit 0 if the cron fell by at least pct% (default 0) vs baseline (prev tick by default; avg over window if given)",
		cronOps(1, 3, "CRON", "PCT", "WINDOW"), nil,
		numericPredicate(wf, "weft::is_falling", predicateFalling(wf)))
	pred("weft::is_outlier", "exit 0 if the cron's latest value is an outlier against a window",
		"weft::is_outlier <cron> <n_stddev> <window> [--above|--below]: exit 0 if |latest - avg(window)| > n_stddev * stddev(window). --above/--below restricts to one direction.",
		cronOps(3, 3, "CRON", "N_STDDEV", "WINDOW"), direction,
		numericPredicate(wf, "weft::is_outlier", predicateOutlier(wf)))
	pred("weft::is_seasonal", "exit 0 if the cron's latest value is unusual for the time of period",
		"weft::is_seasonal <cron> <n_stddev> <period> <window>: exit 0 if latest is n_stddev away from same-time-of-period mean over window (e.g. period=1d window=4w compares to same hour over last 4 days)",
		cronOps(4, 4, "CRON", "N_STDDEV", "PERIOD", "WINDOW"), nil,
		numericPredicate(wf, "weft::is_seasonal", predicateSeasonal(wf)))

	// Stdin comparators — read a float from stdin, compare to threshold.
	// Let users pipe an aggregate value through a predicate:
	//   weft::avg foo 5m | weft::is_gt 50 && ...
	pred("weft::is_gt", "exit 0 if the number on stdin is greater than THRESHOLD", "", cronOps(1, 1, "THRESHOLD"), nil,
		stdinComparator(func(x, t float64) bool { return x > t }))
	pred("weft::is_lt", "exit 0 if the number on stdin is less than THRESHOLD", "", cronOps(1, 1, "THRESHOLD"), nil,
		stdinComparator(func(x, t float64) bool { return x < t }))
}

// numericPredicate refuses a text cron before run: exit 2 with a message,
// like a usage error, since a predicate otherwise answers "can't tell"
// (exit 1) and a type mistake would look like the condition not holding.
func numericPredicate(wf *WeftFuncs, fn string, run func([]string, io.Reader, io.Writer, io.Writer) int) func([]string, io.Reader, io.Writer, io.Writer) int {
	return func(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
		if len(args) > 0 {
			if err := wf.CheckNumeric(fn, args[0]); err != nil {
				fmt.Fprintln(stderr, err)
				return 2
			}
		}
		return run(args, stdin, stdout, stderr)
	}
}

// predicateUsage prints a usage line for a predicate command and returns 2 —
// distinguishable from the "predicate false" exit code (1) so a misuse
// doesn't silently look like "condition not met."
func predicateUsage(stderr io.Writer, msg string) int {
	fmt.Fprintln(stderr, msg)
	return 2
}

func parseFloatArg(arg, label string, stderr io.Writer) (float64, bool) {
	v, err := strconv.ParseFloat(arg, 64)
	if err != nil {
		fmt.Fprintf(stderr, "weft predicate: invalid %s %q\n", label, arg)
		return 0, false
	}
	return v, true
}

func parseDurArg(arg, label string, stderr io.Writer) (time.Duration, bool) {
	d, err := parseDuration(arg)
	if err != nil || d <= 0 {
		fmt.Fprintf(stderr, "weft predicate: invalid %s %q\n", label, arg)
		return 0, false
	}
	return d, true
}

// latestFloat parses the cron's latest stdout as a float. Returns
// (value, true) on success, (0, false) on absent / non-numeric — the
// predicates treat both as "condition can't fire."
func latestFloat(wf *WeftFuncs, watch string) (float64, bool) {
	s, err := wf.Latest(watch)
	if err != nil || s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func predicateAbove(wf *WeftFuncs) func([]string, io.Reader, io.Writer, io.Writer) int {
	return func(args []string, _ io.Reader, _ io.Writer, stderr io.Writer) int {
		if len(args) < 2 {
			return predicateUsage(stderr, "usage: weft::is_above <cron> <threshold> [<window> [--average]]")
		}
		threshold, ok := parseFloatArg(args[1], "threshold", stderr)
		if !ok {
			return 2
		}
		// Windowed forms.
		if len(args) >= 3 {
			window, ok := parseDurArg(args[2], "window", stderr)
			if !ok {
				return 2
			}
			useAvg := len(args) >= 4 && args[3] == "--average"
			if useAvg {
				v, err := wf.Avg(args[0], window)
				if err != nil {
					return 1
				}
				return boolExit(v > threshold)
			}
			// Default: every sample in the window is above (min > threshold).
			v, err := wf.Min(args[0], window)
			if err != nil {
				return 1
			}
			return boolExit(v > threshold)
		}
		v, ok := latestFloat(wf, args[0])
		if !ok {
			return 1
		}
		return boolExit(v > threshold)
	}
}

func predicateBelow(wf *WeftFuncs) func([]string, io.Reader, io.Writer, io.Writer) int {
	return func(args []string, _ io.Reader, _ io.Writer, stderr io.Writer) int {
		if len(args) < 2 {
			return predicateUsage(stderr, "usage: weft::is_below <cron> <threshold> [<window> [--average]]")
		}
		threshold, ok := parseFloatArg(args[1], "threshold", stderr)
		if !ok {
			return 2
		}
		if len(args) >= 3 {
			window, ok := parseDurArg(args[2], "window", stderr)
			if !ok {
				return 2
			}
			useAvg := len(args) >= 4 && args[3] == "--average"
			if useAvg {
				v, err := wf.Avg(args[0], window)
				if err != nil {
					return 1
				}
				return boolExit(v < threshold)
			}
			v, err := wf.Max(args[0], window)
			if err != nil {
				return 1
			}
			return boolExit(v < threshold)
		}
		v, ok := latestFloat(wf, args[0])
		if !ok {
			return 1
		}
		return boolExit(v < threshold)
	}
}

func predicateAbsent(wf *WeftFuncs) func([]string, io.Reader, io.Writer, io.Writer) int {
	return func(args []string, _ io.Reader, _ io.Writer, stderr io.Writer) int {
		if len(args) < 1 {
			return predicateUsage(stderr, "usage: weft::is_absent <cron> [<window>]")
		}
		if len(args) >= 2 {
			window, ok := parseDurArg(args[1], "window", stderr)
			if !ok {
				return 2
			}
			absent, err := wf.Absent(args[0], window)
			if err != nil {
				// Can't tell, so don't fire — but say why: a switch that
				// quietly never fires because its window is over the cap
				// looks exactly like a healthy one.
				fmt.Fprintf(stderr, "weft::is_absent: %v\n", err)
				return 1
			}
			return boolExit(absent)
		}
		absent, err := wf.Absent(args[0], 0)
		if err != nil {
			fmt.Fprintf(stderr, "weft::is_absent: %v\n", err)
			return 1
		}
		return boolExit(absent)
	}
}

func predicateChanged(wf *WeftFuncs) func([]string, io.Reader, io.Writer, io.Writer) int {
	return func(args []string, _ io.Reader, _ io.Writer, stderr io.Writer) int {
		if len(args) < 1 {
			return predicateUsage(stderr, "usage: weft::has_changed <cron>")
		}
		latest, err := wf.Latest(args[0])
		if err != nil {
			return 1
		}
		prev, err := wf.Previous(args[0])
		if err != nil || prev == "" {
			// No previous tick to compare against — first run is not
			// "changed". This matches alert.Changed's behavior.
			return 1
		}
		return boolExit(latest != prev)
	}
}

func predicateRising(wf *WeftFuncs) func([]string, io.Reader, io.Writer, io.Writer) int {
	return func(args []string, _ io.Reader, _ io.Writer, stderr io.Writer) int {
		if len(args) < 1 {
			return predicateUsage(stderr, "usage: weft::is_rising <cron> [<pct>] [<window>]")
		}
		return evalTrend(wf, args, true, stderr)
	}
}

func predicateFalling(wf *WeftFuncs) func([]string, io.Reader, io.Writer, io.Writer) int {
	return func(args []string, _ io.Reader, _ io.Writer, stderr io.Writer) int {
		if len(args) < 1 {
			return predicateUsage(stderr, "usage: weft::is_falling <cron> [<pct>] [<window>]")
		}
		return evalTrend(wf, args, false, stderr)
	}
}

// evalTrend handles rising/falling. rising=true compares (current-baseline)/baseline
// against +pct; rising=false against -pct. Baseline is avg(window) when window given,
// else previous tick.
func evalTrend(wf *WeftFuncs, args []string, rising bool, stderr io.Writer) int {
	current, ok := latestFloat(wf, args[0])
	if !ok {
		return 1
	}
	pct := 0.0
	if len(args) >= 2 {
		v, ok := parseFloatArg(args[1], "pct", stderr)
		if !ok {
			return 2
		}
		pct = v / 100.0
	}
	var baseline float64
	if len(args) >= 3 {
		window, ok := parseDurArg(args[2], "window", stderr)
		if !ok {
			return 2
		}
		v, err := wf.Avg(args[0], window)
		if err != nil {
			return 1
		}
		baseline = v
	} else {
		prev, err := wf.Previous(args[0])
		if err != nil || prev == "" {
			return 1
		}
		v, err := strconv.ParseFloat(prev, 64)
		if err != nil {
			return 1
		}
		baseline = v
	}
	if baseline == 0 {
		// Can't compute percent change against zero; treat as not-firing
		// rather than divide-by-zero. Matches alert.Rising's `baseline > 0`
		// gate.
		return 1
	}
	pctChange := (current - baseline) / baseline
	if rising {
		return boolExit(pctChange >= pct)
	}
	return boolExit(-pctChange >= pct)
}

func predicateOutlier(wf *WeftFuncs) func([]string, io.Reader, io.Writer, io.Writer) int {
	return func(args []string, _ io.Reader, _ io.Writer, stderr io.Writer) int {
		if len(args) < 3 {
			return predicateUsage(stderr, "usage: weft::is_outlier <cron> <n_stddev> <window> [--above|--below]")
		}
		nSigma, ok := parseFloatArg(args[1], "n_stddev", stderr)
		if !ok {
			return 2
		}
		window, ok := parseDurArg(args[2], "window", stderr)
		if !ok {
			return 2
		}
		direction := ""
		if len(args) >= 4 {
			switch args[3] {
			case "--above", "--below":
				direction = args[3]
			default:
				return predicateUsage(stderr, "weft::is_outlier: unknown flag "+args[3]+" (expected --above or --below)")
			}
		}
		current, ok := latestFloat(wf, args[0])
		if !ok {
			return 1
		}
		avg, err := wf.Avg(args[0], window)
		if err != nil {
			return 1
		}
		stddev, err := wf.Stddev(args[0], window)
		if err != nil {
			return 1
		}
		// Zero-stddev guard: a perfectly flat history would fire on
		// any departure at all. Floor at 1% of |avg| (with a hard 1e-9
		// minimum) — below that we have no signal, so no fire.
		floor := 0.01 * absFloat(avg)
		if floor < 1e-9 {
			floor = 1e-9
		}
		if stddev < floor {
			return 1
		}
		delta := current - avg
		switch direction {
		case "--above":
			return boolExit(delta > nSigma*stddev)
		case "--below":
			return boolExit(-delta > nSigma*stddev)
		default:
			return boolExit(absFloat(delta) > nSigma*stddev)
		}
	}
}

func predicateSeasonal(wf *WeftFuncs) func([]string, io.Reader, io.Writer, io.Writer) int {
	return func(args []string, _ io.Reader, _ io.Writer, stderr io.Writer) int {
		if len(args) < 4 {
			return predicateUsage(stderr, "usage: weft::is_seasonal <cron> <n_stddev> <period> <window>")
		}
		nSigma, ok := parseFloatArg(args[1], "n_stddev", stderr)
		if !ok {
			return 2
		}
		period, ok := parseDurArg(args[2], "period", stderr)
		if !ok {
			return 2
		}
		window, ok := parseDurArg(args[3], "window", stderr)
		if !ok {
			return 2
		}
		if period >= window {
			fmt.Fprintln(stderr, "weft::is_seasonal: window must be larger than period")
			return 2
		}
		current, ok := latestFloat(wf, args[0])
		if !ok {
			return 1
		}
		stats, err := wf.SeasonalStats(args[0], period, window)
		if err != nil {
			return 1
		}
		// Need at least 4 same-time samples; below that the stddev
		// estimate is too noisy to trust. Matches the anomaly
		// condition's min_samples=4.
		if stats.Samples < 4 {
			return 1
		}
		floor := 0.01 * absFloat(stats.Avg)
		if floor < 1e-9 {
			floor = 1e-9
		}
		if stats.Stddev < floor {
			return 1
		}
		return boolExit(absFloat(current-stats.Avg) > nSigma*stats.Stddev)
	}
}

// stdinComparator builds a `weft::is_gt`/`weft::is_lt`-style filter
// that reads one float from stdin and compares against the threshold
// argument. Empty / non-numeric stdin returns 1 (the comparison
// can't be made).
func stdinComparator(cmp func(value, threshold float64) bool) func([]string, io.Reader, io.Writer, io.Writer) int {
	return func(args []string, stdin io.Reader, _ io.Writer, stderr io.Writer) int {
		if len(args) < 1 {
			fmt.Fprintln(stderr, "usage: weft::is_gt|weft::is_lt <threshold>")
			return 2
		}
		threshold, ok := parseFloatArg(args[0], "threshold", stderr)
		if !ok {
			return 2
		}
		scanner := bufio.NewScanner(stdin)
		scanner.Buffer(make([]byte, 4096), 64*1024)
		if !scanner.Scan() {
			return 1
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			return 1
		}
		v, err := strconv.ParseFloat(line, 64)
		if err != nil {
			return 1
		}
		return boolExit(cmp(v, threshold))
	}
}

func boolExit(b bool) int {
	if b {
		return 0
	}
	return 1
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
