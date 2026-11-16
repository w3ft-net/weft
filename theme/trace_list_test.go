package theme

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

var listNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func listLine(id string, endAgo, dur time.Duration, lines int, errors string, services, root string) string {
	end := listNow.Add(-endAgo)
	start := end.Add(-dur)
	return `{"trace_id":"` + id + `","start_ns":` + itoa(start.UnixNano()) + `,"end_ns":` + itoa(end.UnixNano()) +
		`,"dur_ns":` + itoa(dur.Nanoseconds()) + `,"lines":` + itoa(int64(lines)) + `,"errors":` + errors +
		`,"services":[` + services + `],"root":"` + root + `"}`
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestParseTraceList(t *testing.T) {
	rows := ParseTraceList(listNow, []string{
		listLine("t1", 3*time.Minute, 1200*time.Millisecond, 7, "2", `"acme/api","acme/orders"`, "GET /orders/812"),
		listLine("t2", 20*time.Second, 5*time.Millisecond, 1, "null", `"acme/api"`, ""),
		"not json",
		"",
	})
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %+v", rows)
	}
	if rows[0].TraceID != "t1" || rows[0].Lines != 7 || rows[0].Errors != 2 || rows[0].NoLevel ||
		rows[0].Root != "GET /orders/812" || strings.Join(rows[0].Services, ",") != "acme/api,acme/orders" {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[0].Dur != "1.2s" || rows[0].Age != "3m" {
		t.Errorf("row 0 Dur/Age = %q/%q", rows[0].Dur, rows[0].Age)
	}
	if !rows[1].NoLevel {
		t.Errorf("a null errors count means the level is unknown: %+v", rows[1])
	}
}

func TestFormatAge(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"}, {42 * time.Second, "42s"}, {3 * time.Minute, "3m"}, {90 * time.Minute, "1h"},
		{5 * time.Hour, "5h"}, {49 * time.Hour, "2d"},
	} {
		if got := formatAge(c.d); got != c.want {
			t.Errorf("formatAge(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestRenderTraceList_Columns(t *testing.T) {
	out := RenderTraceList(listNow, []string{
		listLine("4bf92f3577b34da6a3ce929d0e0e4736", 3*time.Minute, 1200*time.Millisecond, 7, "2", `"acme/api","acme/orders"`, "GET /orders/812"),
		listLine("aaaa", 20*time.Second, 5*time.Millisecond, 1, "null", `"acme/api"`, ""),
	}, 120)
	if len(out) != 3 {
		t.Fatalf("want a header and 2 rows, got %d: %q", len(out), out)
	}
	head := stripANSI(out[0])
	for _, h := range []string{"TRACE", "AGE", "DUR", "LINES", "ERRORS", "ROOT", "SERVICES"} {
		if !strings.Contains(head, h) {
			t.Errorf("header missing %q: %q", h, head)
		}
	}
	r1 := stripANSI(out[1])
	for _, want := range []string{"4bf92f3577b34da6a3ce929d0e0e4736", "3m", "1.2s", "GET /orders/812", "acme/api, acme/orders"} {
		if !strings.Contains(r1, want) {
			t.Errorf("row 1 missing %q: %q", want, r1)
		}
	}
	if f := strings.Fields(r1); f[3] != "2" || f[len(f)-1] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("row 1 ERRORS = %q (want 2) and the ID should be last: %q", f[3], r1)
	}
	r2 := stripANSI(out[2])
	if !strings.Contains(r2, "20s") || !strings.Contains(r2, "-") {
		t.Errorf("row 2 should show 20s and a - for unknown errors: %q", r2)
	}
}

func TestRenderTraceList_KeepsTheFullIDAndFitsTheWidth(t *testing.T) {
	id := "4bf92f3577b34da6a3ce929d0e0e4736"
	out := RenderTraceList(listNow, []string{
		listLine(id, time.Minute, time.Second, 3, "0", `"acme/api","acme/orders","acme/payments","acme/inventory"`, "POST /checkout/session/confirm"),
	}, 100)
	for i, l := range out {
		if w := len([]rune(stripANSI(l))); w > 100 {
			t.Errorf("line %d is %d wide, over 100: %q", i, w, stripANSI(l))
		}
	}
	if !strings.Contains(stripANSI(out[1]), id) {
		t.Errorf("the ID is what you pass to --id, so it is never cut: %q", stripANSI(out[1]))
	}
}

func TestRenderTraceList_NarrowTerminalStillShowsIDs(t *testing.T) {
	id := "4bf92f3577b34da6a3ce929d0e0e4736"
	out := RenderTraceList(listNow, []string{listLine(id, time.Minute, time.Second, 3, "0", `"acme/api"`, "GET /")}, 40)
	if len(out) != 2 || !strings.Contains(stripANSI(out[1]), id) {
		t.Fatalf("got %q", out)
	}
}

func TestRenderTraceList_Empty(t *testing.T) {
	out := RenderTraceList(listNow, nil, 80)
	if len(out) != 1 || !strings.Contains(stripANSI(out[0]), "no traces") {
		t.Fatalf("got %q", out)
	}
}

// At the default 80 columns the root request name is what a person scans for,
// so it is kept ahead of the services.
func TestRenderTraceList_EightyColumnsKeepsTheRoot(t *testing.T) {
	id := "4bf92f3577b34da6a3ce929d0e0e4736"
	out := RenderTraceList(listNow, []string{
		listLine(id, time.Minute, time.Second, 3, "0", `"acme/api","acme/orders"`, "GET /orders/812"),
	}, 80)
	row := stripANSI(out[1])
	if !strings.Contains(row, "GET /orders") || !strings.Contains(row, id) {
		t.Errorf("want the root and the full ID at 80 columns: %q", row)
	}
	if w := len([]rune(row)); w > 80 {
		t.Errorf("row is %d wide: %q", w, row)
	}
}
