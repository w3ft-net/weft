package chart

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseChartCmd(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want WatchArgs
		err  string // substring match; "" = expect no error
	}{
		{
			name: "bare cron name",
			argv: []string{"errors"},
			want: WatchArgs{Name: "errors", Kind: "line", Aggregate: "avg", Interval: time.Second},
		},
		{
			name: "path form strips /cron.d/",
			argv: []string{"/cron.d/errors"},
			want: WatchArgs{Name: "errors", Kind: "line", Aggregate: "avg", Interval: time.Second},
		},
		{
			name: "type bar",
			argv: []string{"errors", "-t", "bar"},
			want: WatchArgs{Name: "errors", Kind: "bar", Aggregate: "avg", Interval: time.Second},
		},
		{
			name: "aggregate, bucket and title",
			argv: []string{"errors", "-a", "max", "--bucket", "1h", "--title", "peak"},
			want: WatchArgs{Name: "errors", Kind: "line", Aggregate: "max", Bucket: time.Hour, Title: "peak", Interval: time.Second},
		},
		{
			name: "interval + follow",
			argv: []string{"errors", "-i", "500ms", "-f"},
			want: WatchArgs{Name: "errors", Kind: "line", Aggregate: "avg", Interval: 500 * time.Millisecond, Follow: true},
		},
		{name: "missing cron name", argv: []string{"-f"}, err: "usage: chart"},
		{name: "-n is gone: the window is the range", argv: []string{"errors", "-n", "20"}, err: "chart: unknown option -n"},
		{name: "-x is pipe-only", argv: []string{"errors", "-x", "1"}, err: "chart: unknown option -x"},
		{name: "unknown flag", argv: []string{"errors", "--no-such"}, err: "chart: unknown option --no-such"},
		{name: "bad type", argv: []string{"errors", "-t", "candlestick"}, err: "unknown type"},
		{name: "bad aggregate", argv: []string{"errors", "-a", "identity"}, err: "unknown --aggregate"},
		{name: "interval too short", argv: []string{"errors", "-i", "10ms"}, err: "duration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, errMsg := ParseWatchCmd(tc.argv)
			if tc.err != "" {
				if !strings.Contains(errMsg, tc.err) {
					t.Errorf("expected error containing %q, got %q", tc.err, errMsg)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("unexpected error: %s", errMsg)
			}
			if got != tc.want {
				t.Errorf("parsed = %+v\n   want %+v", got, tc.want)
			}
		})
	}
}

var t0 = time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

func raw(min int, tag string, v float64) CronBucket {
	return CronBucket{Start: t0.Add(time.Duration(min) * time.Minute), Tag: tag, Count: 1, Sum: v, Min: v, Max: v}
}

func plotOf(t *testing.T, series CronSeries, args WatchArgs) map[string][]float64 {
	t.Helper()
	plot, msg := CronPlot(series, args)
	if msg != "" {
		t.Fatal(msg)
	}
	out := map[string][]float64{}
	for _, s := range plot.Series {
		for _, p := range s.Points {
			out[s.Tag] = append(out[s.Tag], p.Y)
		}
	}
	return out
}

func TestCronPlot_OneLinePerTag(t *testing.T) {
	series := CronSeries{Granularity: "raw", Buckets: []CronBucket{
		raw(0, "api-1", 3), raw(0, "api-2", 4),
		raw(5, "api-1", 5), raw(5, "api-2", 6),
		raw(5, "api-1", 7), // a duplicate tag in one run
	}}
	got := plotOf(t, series, WatchArgs{Aggregate: "avg"})
	if fmt.Sprint(got) != fmt.Sprint(map[string][]float64{"api-1": {3, 6}, "api-2": {4, 6}}) {
		t.Fatalf("avg = %v", got)
	}
	got = plotOf(t, series, WatchArgs{Aggregate: "count", Bucket: time.Hour})
	if fmt.Sprint(got) != fmt.Sprint(map[string][]float64{"api-1": {3}, "api-2": {2}}) {
		t.Fatalf("count per hour = %v", got)
	}
}

func TestCronPlot_FoldsTagsPastTheTop(t *testing.T) {
	var buckets []CronBucket
	for i := 0; i < foldTop+3; i++ {
		buckets = append(buckets, raw(0, fmt.Sprintf("h%02d", i), float64(100-i)))
	}
	plot, msg := CronPlot(CronSeries{Granularity: "raw", Buckets: buckets}, WatchArgs{Aggregate: "sum"})
	if msg != "" {
		t.Fatal(msg)
	}
	if len(plot.Series) != foldTop+1 {
		t.Fatalf("%d series, want %d named + other", len(plot.Series), foldTop)
	}
	last := plot.Series[len(plot.Series)-1]
	// The three smallest (100-8, 100-9, 100-10) fold into other.
	if last.Tag != otherTag || last.Points[0].Y != 92+91+90 {
		t.Fatalf("last series = %+v, want other = 273", last)
	}
}

func TestCronPlot_RollupLimits(t *testing.T) {
	hourly := CronSeries{Granularity: "hourly", Buckets: []CronBucket{{Start: t0, Count: 4, Sum: 10, Min: 1, Max: 4}}}
	if _, msg := CronPlot(hourly, WatchArgs{Aggregate: "p95"}); !strings.Contains(msg, "-a p95 needs every run") {
		t.Fatalf("p95 over rollups: %q", msg)
	}
	if _, msg := CronPlot(hourly, WatchArgs{Aggregate: "avg", Bucket: 5 * time.Minute}); !strings.Contains(msg, "finer than the hourly rollups") {
		t.Fatalf("5m bucket over hourly rollups: %q", msg)
	}
	for agg, want := range map[string]float64{"count": 4, "sum": 10, "avg": 2.5, "min": 1, "max": 4} {
		if got := plotOf(t, hourly, WatchArgs{Aggregate: agg})[""][0]; got != want {
			t.Errorf("%s over a rollup = %v, want %v", agg, got, want)
		}
	}
	rawSeries := CronSeries{Granularity: "raw", Buckets: []CronBucket{raw(0, "", 1), raw(1, "", 2), raw(2, "", 9)}}
	if got := plotOf(t, rawSeries, WatchArgs{Aggregate: "p50", Bucket: time.Hour})[""][0]; got != 2 {
		t.Errorf("p50 over raw runs = %v, want 2", got)
	}
}

func TestCheckFollow(t *testing.T) {
	if msg := CheckFollow("2h.."); msg != "" {
		t.Errorf("relative range refused: %s", msg)
	}
	if msg := CheckFollow("2026-01-01..2026-01-02"); !strings.Contains(msg, "--follow needs a range that runs to now") {
		t.Errorf("past range: %q", msg)
	}
}

func TestRenderCron_BarRefusesTags(t *testing.T) {
	series := CronSeries{Granularity: "raw", Buckets: []CronBucket{raw(0, "a", 1), raw(0, "b", 2)}}
	if _, msg := RenderCron("hosts", series, WatchArgs{Kind: "bar", Aggregate: "avg"}, 80, 20); !strings.Contains(msg, "has 2 tags; use -t line") {
		t.Fatalf("bar over tags: %q", msg)
	}
	out, msg := RenderCron("hosts", series, WatchArgs{Kind: "sparkline", Aggregate: "avg"}, 80, 20)
	if msg != "" || !strings.HasPrefix(out, "a ") || !strings.Contains(out, "\nb ") {
		t.Fatalf("tagged sparkline should draw one labelled line per tag: %q %q", out, msg)
	}
}
