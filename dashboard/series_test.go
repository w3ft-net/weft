package dashboard

import (
	"testing"
	"time"

	"github.com/graybeardcorp/weft/chart"
)

func bucket(min int, tag string, v float64) chart.CronBucket {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return chart.CronBucket{Start: t0.Add(time.Duration(min) * time.Minute), Tag: tag, Count: 1, Sum: v, Min: v, Max: v}
}

// A tagged cron's sparkline reads the tags the cron stored: one sub-series
// per tag, newest-first like the untagged path.
func TestToWatchSeries_OneSeriesPerStoredTag(t *testing.T) {
	ws, msg := toWatchSeries(chart.CronSeries{Granularity: "raw", Buckets: []chart.CronBucket{
		bucket(0, "i-aaa", 40), bucket(0, "i-bbb", 60),
		bucket(1, "i-aaa", 44), bucket(1, "i-bbb", 64),
		bucket(2, "i-aaa", 48), bucket(2, "i-bbb", 68),
	}})
	if msg != "" {
		t.Fatal(msg)
	}
	if len(ws.Tags) != 2 {
		t.Fatalf("expected 2 tags, got %+v", ws.Tags)
	}
	if aaa := ws.Tags["i-aaa"].Values; len(aaa) != 3 || aaa[0] != 48 || aaa[2] != 40 {
		t.Errorf("i-aaa values: got %v, want newest-first [48 44 40]", aaa)
	}
}

func TestToWatchSeries_Untagged(t *testing.T) {
	ws, msg := toWatchSeries(chart.CronSeries{Granularity: "raw", Buckets: []chart.CronBucket{bucket(0, "", 1), bucket(1, "", 2)}})
	if msg != "" || ws.Tags != nil || len(ws.Values) != 2 || ws.Values[0] != 2 {
		t.Fatalf("ws=%+v msg=%q, want newest-first [2 1] with no tags", ws, msg)
	}
}
