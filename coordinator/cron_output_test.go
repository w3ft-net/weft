package coordinator

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/store"
)

func mustSpec(t *testing.T, s string) shellcore.FieldSpec {
	t.Helper()
	f, err := shellcore.ParseFieldSpec(s)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestParseCronOutput(t *testing.T) {
	text := cronSchema{}
	flagless := cronSchema{alerts: true}
	positional := cronSchema{value: mustSpec(t, "3"), tag: mustSpec(t, "1"), alerts: true}
	headers := cronSchema{value: mustSpec(t, ".pct"), tag: mustSpec(t, ".host")}
	jsonAlert := cronSchema{value: mustSpec(t, ".pct"), alerts: true}

	type pt struct {
		value float64
		tag   string
	}
	cases := []struct {
		name   string
		schema cronSchema
		stdout string
		want   []pt
		strike string
	}{
		{"empty", text, "\n  \n", nil, ""},
		{"one number", text, "42\n", []pt{{42, ""}}, ""},
		{"value then tag", text, "42 api-1\n7 api-2\n", []pt{{42, "api-1"}, {7, "api-2"}}, ""},
		{"uniq -c shape", text, "     42 api-1\n", []pt{{42, "api-1"}}, ""},
		{"duplicate tags", text, "1 a\n2 a\n", []pt{{1, "a"}, {2, "a"}}, ""},
		{"check 1: three fields", text, "1 a\n2 b\n3 c extra\n", nil, "line 3 has 3 fields; text output is <value> [<tag>]"},
		{"check 3: JSON without schema", text, `{"pct": 5}`, nil, "output is JSON; add # value: (and # tag:) headers, or alert -v"},
		{"check 4: text value", text, "n/a api-1\n", nil, `line 1: value "n/a" is not a number`},

		{"flagless alert number", flagless, "5\n", []pt{{5, ""}}, ""},
		{"check 6: tag reaches flagless alert", flagless, "5 api-1\n", nil, "line 1 has a tag; give alert -v 1 -t 2"},
		{"check 6: JSON reaches flagless alert", flagless, `{"pct": 5}`, nil, "output is JSON; give alert -v <path>"},
		{"flagless alert reads one line", flagless, "1\n2\n", nil, "output has 2 lines"},

		{"positional ignores the two-field limit", positional, "web1 x 81 y z\n", []pt{{81, "web1"}}, ""},
		{"positional missing value", positional, "web1 x\n", nil, "line 1 has no field 3 for alert -v"},

		{"headers", headers, `{"host":"a","pct":5}` + "\n" + `{"host":"b","pct":6.5}`, []pt{{5, "a"}, {6.5, "b"}}, ""},
		{"missing tag stores untagged", headers, `{"host":null,"pct":5}` + "\n" + `{"pct":6}`, []pt{{5, ""}, {6, ""}}, ""},
		{"check 2: text under JSON schema", headers, "5 a\n", nil, "line 1 is not JSON, but the cron declares # value: .pct"},
		{"check 2 names alert -v", jsonAlert, "5\n", nil, "line 1 is not JSON, but the cron declares alert -v .pct"},
		{"check 4: JSON value not a number", headers, `{"host":"a","pct":"n/a"}`, nil, `line 1: value "n/a" is not a number`},
		{"check 4: JSON value missing", headers, `{"host":"a"}`, nil, "line 1 has no .pct"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			points, err := parseCronOutput(c.schema, c.stdout)
			if c.strike != "" {
				if err == nil || !strings.Contains(err.Error(), c.strike) {
					t.Fatalf("err = %v, want an error containing %q", err, c.strike)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := make([]pt, len(points))
			for i, p := range points {
				got[i] = pt{p.value, p.tag}
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Fatalf("points = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCheckTagCap(t *testing.T) {
	var points []cronPoint
	for i := 0; i < 11; i++ {
		points = append(points, cronPoint{tag: fmt.Sprint(i)}, cronPoint{tag: fmt.Sprint(i)})
	}
	points = append(points, cronPoint{})
	err := checkTagCap(points, 10)
	if err == nil || err.Error() != "run printed 11 tags; your plan allows 10 per run" {
		t.Fatalf("err = %v", err)
	}
	if err := checkTagCap(points, 11); err != nil {
		t.Fatalf("11 tags under a cap of 11: %v", err)
	}
}

// TestCronOutputDisables runs crons through the executor: output that breaks
// the contract, or prints more tags than the plan allows, stores no points
// and disables the cron on its first run with a page that names the check.
func TestCronOutputDisables(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	if err := db.SaveShellFunction(custID, "", "wide", `echo "1 a extra"`, "bash"); err != nil {
		t.Fatal(err)
	}
	// Eleven tags; a trial org may print ten per run.
	if err := db.SaveShellFunction(custID, "", "many", `printf '1 h1\n1 h2\n1 h3\n1 h4\n1 h5\n1 h6\n1 h7\n1 h8\n1 h9\n1 h10\n1 h11\n'`, "bash"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveShellFunction(custID, "", "hosts", `printf '3 api-1\n4 api-2\n'`, "bash"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"wide", "many", "hosts"} {
		if err := db.SaveCronWatch(custID, "", name, "every 5m", name, true, nil); err != nil {
			t.Fatal(err)
		}
	}

	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	var pages []string
	ce := &CronExecutor{Store: db, Coord: c, RunTimeout: 5 * time.Second,
		SendSystem: func(_ context.Context, _ store.OrgID, _, _, body string) error {
			pages = append(pages, body)
			return nil
		}}
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	run := func(name string) store.CronWatch {
		t.Helper()
		w, err := db.GetCronWatch(custID, name)
		if err != nil {
			t.Fatal(err)
		}
		ce.executeWatch(context.Background(), *w, tr)
		w, _ = db.GetCronWatch(custID, name)
		return *w
	}

	for _, c := range []struct{ name, check string }{
		{"wide", "line 1 has 3 fields"},
		{"many", "run printed 11 tags"},
	} {
		pages = nil
		if w := run(c.name); w.LastStatus != "invalid" || w.Enabled || w.ConsecutiveStrikes != 0 {
			t.Fatalf("%s: status=%q enabled=%v strikes=%d, want invalid, disabled, no strike", c.name, w.LastStatus, w.Enabled, w.ConsecutiveStrikes)
		}
		if len(pages) != 1 || !strings.Contains(pages[0], c.check) {
			t.Fatalf("%s: pages = %q, want one naming %q", c.name, pages, c.check)
		}
	}
	w, _ := db.GetCronWatch(custID, "wide")
	rows, err := db.ListCronResults(w.ID, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Value != nil || r.Stdout != "" || !strings.Contains(r.Stderr, "3 fields") {
			t.Fatalf("an invalid run stored %+v; want only the reason it was disabled, no value", r)
		}
	}

	if w := run("hosts"); w.LastStatus != "ok" || w.ConsecutiveStrikes != 0 {
		t.Fatalf("hosts: status=%q strikes=%d", w.LastStatus, w.ConsecutiveStrikes)
	}
	hw, _ := db.GetCronWatch(custID, "hosts")
	for tag, want := range map[string]float64{"api-1": 3, "api-2": 4} {
		rows, err := db.ListCronResults(hw.ID, 1, tag)
		if err != nil || len(rows) != 1 || rows[0].Value == nil || *rows[0].Value != want {
			t.Fatalf("tag %s: rows=%+v err=%v, want value %v", tag, rows, err, want)
		}
	}

	// Schema headers ride along in the pipeline as comments and still run.
	if err := db.SaveCronWatch(custID, "", "json", "every 5m",
		"# value: .pct\n# tag: .host\necho '{\"host\":\"db-1\",\"pct\":12.5}'", true, nil); err != nil {
		t.Fatal(err)
	}
	if w := run("json"); w.LastStatus != "ok" {
		t.Fatalf("json: status=%q", w.LastStatus)
	}
	jw, _ := db.GetCronWatch(custID, "json")
	if rows, err := db.ListCronResults(jw.ID, 1, "db-1"); err != nil || len(rows) != 1 || *rows[0].Value != 12.5 {
		t.Fatalf("json rows=%+v err=%v, want db-1 = 12.5", rows, err)
	}
}
