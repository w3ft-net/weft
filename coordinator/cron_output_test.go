package coordinator

import (
	"fmt"
	"github.com/w3ft-net/weft/fieldspec"
	"strings"
	"testing"

)

func mustSpec(t *testing.T, s string) fieldspec.FieldSpec {
	t.Helper()
	f, err := fieldspec.ParseFieldSpec(s)
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
