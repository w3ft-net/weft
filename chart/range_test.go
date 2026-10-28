package chart

import (
	"testing"
	"time"
)

// With no -y the value is field 1, as a cron stores `<value> [<tag>]`; after
// -x 1 it is the field after x.
func TestPlotFromStream_DefaultYIsTheValueField(t *testing.T) {
	args, msg := parsePipeArgs([]string{"--kind", "line"})
	if msg != "" {
		t.Fatal(msg)
	}
	plot, msg := PlotFromStream([]string{"42 api-1", "7 api-2"}, args)
	if msg != "" {
		t.Fatal(msg)
	}
	if pts := plot.Series[0].Points; pts[0].Y != 42 || pts[1].Y != 7 {
		t.Fatalf("points = %+v, want y from field 1", pts)
	}

	args, _ = parsePipeArgs([]string{"-x", "1", "--kind", "line"})
	plot, msg = PlotFromStream([]string{"2026-01-01T00:00:00Z 5 x", "2026-01-01T00:01:00Z 6 x"}, args)
	if msg != "" || plot.Series[0].Points[0].Y != 5 {
		t.Fatalf("after -x 1: plot=%+v msg=%q, want y from field 2", plot, msg)
	}
}

// With a time x-axis, zero-filled buckets and the axis reach both ends of
// the range, not just first-to-last row.
func TestPlotFromStream_SpansTheRange(t *testing.T) {
	args, msg := parsePipeArgs([]string{"-x", "1", "--bucket", "1h"})
	if msg != "" {
		t.Fatal(msg)
	}
	args.RangeStart = time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC)
	args.RangeEnd = time.Date(2026, 1, 1, 6, 15, 0, 0, time.UTC)
	plot, msg := PlotFromStream([]string{"2026-01-01T02:10:00Z a", "2026-01-01T02:20:00Z b", "2026-01-01T03:05:00Z c"}, args)
	if msg != "" {
		t.Fatal(msg)
	}
	var ys []float64
	for _, p := range plot.Series[0].Points {
		ys = append(ys, p.Y)
	}
	// Buckets 00:00 .. 06:00: zeros either side of the rows at 02:00 and 03:00.
	want := []float64{0, 0, 2, 1, 0, 0, 0}
	if len(ys) != len(want) {
		t.Fatalf("counts = %v, want %v", ys, want)
	}
	for i := range want {
		if ys[i] != want[i] {
			t.Fatalf("counts = %v, want %v", ys, want)
		}
	}
	// The axis starts at the bucket holding the range's start.
	if plot.XMin != float64(args.RangeStart.Truncate(time.Hour).Unix()) || plot.XMax != float64(args.RangeEnd.Unix()) {
		t.Errorf("x span = %v..%v, want 00:00..06:15", time.Unix(int64(plot.XMin), 0).UTC(), time.Unix(int64(plot.XMax), 0).UTC())
	}
}
