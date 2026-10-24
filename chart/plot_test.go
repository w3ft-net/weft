package chart

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPlotFromStream_BucketedCountIsOneTimeSeries(t *testing.T) {
	args, msg := parsePipeArgs([]string{"-x", "1", "--bucket", "5m"})
	if msg != "" {
		t.Fatal(msg)
	}
	plot, errMsg := PlotFromStream([]string{
		"2026-10-10T12:00:01Z ERROR a",
		"2026-10-10T12:02:40Z ERROR b",
		"2026-10-10T12:07:00Z ERROR c",
	}, args)
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	if plot.Kind != "line" || !plot.TimeX || len(plot.Series) != 1 || plot.Series[0].Tag != "" {
		t.Fatalf("plot shape = %+v", plot)
	}
	pts := plot.Series[0].Points
	if len(pts) != 2 || pts[0].Y != 2 || pts[1].Y != 1 {
		t.Fatalf("points = %+v, want counts [2 1]", pts)
	}
	want := float64(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC).Unix())
	if pts[0].X != want || pts[1].X != want+300 {
		t.Errorf("x = %v,%v want unix %v and +300", pts[0].X, pts[1].X, want)
	}
}

func TestPlotFromStream_TagSplitsSeries(t *testing.T) {
	args, msg := parsePipeArgs([]string{"-x", "1", "-y", "3", "--tag", "2", "--bucket", "1m", "-a", "sum"})
	if msg != "" {
		t.Fatal(msg)
	}
	plot, errMsg := PlotFromStream([]string{
		"2026-10-10T12:00:01Z web 1",
		"2026-10-10T12:00:02Z db 5",
		"2026-10-10T12:01:01Z web 2",
	}, args)
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	if len(plot.Series) != 2 || plot.Series[0].Tag != "db" || plot.Series[1].Tag != "web" {
		t.Fatalf("series = %+v, want db then web", plot.Series)
	}
}

func TestPlotFromStream_CategoricalBarKeepsLabels(t *testing.T) {
	args, msg := parsePipeArgs([]string{"-x", "1", "-y", "2", "-a", "sum"})
	if msg != "" {
		t.Fatal(msg)
	}
	plot, errMsg := PlotFromStream([]string{"get 3", "put 4", "get 2"}, args)
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	if plot.Kind != "bar" || plot.TimeX {
		t.Fatalf("plot = %+v", plot)
	}
	pts := plot.Series[0].Points
	if len(pts) != 2 || pts[0].Label != "get" || pts[0].Y != 5 || pts[1].Label != "put" {
		t.Fatalf("points = %+v", pts)
	}
}

func TestPlotFromStream_NoData(t *testing.T) {
	args, _ := parsePipeArgs(nil)
	if _, errMsg := PlotFromStream(nil, args); errMsg != "no data points to chart" {
		t.Errorf("errMsg = %q", errMsg)
	}
}

// TestPlotJSON_EmptyIsArrays: the browser maps over series and points, so a
// cron with nothing stored in the window must send [] rather than null — a
// null there crashed the whole Dashboards page.
func TestPlotJSON_EmptyIsArrays(t *testing.T) {
	for name, p := range map[string]Plot{
		"no series":         {Kind: "line", TimeX: true},
		"series, no points": {Kind: "line", TimeX: true, Series: []PlotSeries{{Tag: "a"}}},
	} {
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "null") {
			t.Errorf("%s: %s", name, b)
		}
	}
	empty, msg := CronPlot(CronSeries{}, WatchArgs{Kind: "line", Aggregate: "avg"})
	if msg != "" {
		t.Fatal(msg)
	}
	if b, _ := json.Marshal(empty); !strings.Contains(string(b), `"series":[]`) {
		t.Errorf("CronPlot of an empty series: %s", b)
	}
}
