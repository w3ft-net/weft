package planner

import (
	"strings"
	"testing"
	"time"
)

// EVENT_ID and LINE_NO push down with a one-line event as line 0 and
// empty id, the way compute binds them.
func TestEventMetaVarsPushDown(t *testing.T) {
	for _, tc := range []struct{ pipeline, want string }{
		{`awk 'LINE_NO == 0'`, `COALESCE(line_no, 0) = 0`},
		{`awk 'LINE_NO > 0'`, `line_no > 0`},
		{`awk 'EVENT_ID == ""'`, `COALESCE(CAST(event_id AS VARCHAR), '') = ''`},
		{`awk '{ print LINE_NO }'`, `CAST(COALESCE(line_no, 0) AS VARCHAR)`},
		{`jq 'select($line_no == 1)'`, `COALESCE(line_no, 0) = 1`},
		{`jq 'select($event_id == "")'`, `COALESCE(CAST(event_id AS VARCHAR), '') = ''`},
	} {
		plan, err := planBare("cat /log/acme/api/main.log | "+tc.pipeline, "", time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("%s: %v", tc.pipeline, err)
		}
		if !strings.Contains(plan.ShardSQL, tc.want) {
			t.Errorf("%s:\nSQL %s\nwant it to contain %s", tc.pipeline, plan.ShardSQL, tc.want)
		}
	}
}

// jq reads numeric columns as numbers (a NULL one as null) and the rest as
// strings; a literal of the other type doesn't lower, and a payload key
// named like a meta-variable is the payload's.
func TestJqMetaVarTypesPushDown(t *testing.T) {
	sqlOf := func(pipeline string) string {
		plan, err := planBare("cat /log/acme/api/main.log | "+pipeline, "", time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("%s: %v", pipeline, err)
		}
		return plan.ShardSQL
	}
	for _, tc := range []struct{ pipeline, want string }{
		{`jq 'select($duration_ns > 1000)'`, `duration_ns > 1000`},
		{`jq 'select($duration_ns < 5)'`, `(duration_ns IS NULL OR duration_ns < 5)`},
		{`jq 'select($level != "x")'`, `(level IS NULL OR level != 'x')`},
		{`jq 'select(.level == "x")'`, `json_extract_string(payload, '$.level') = 'x'`},
	} {
		if got := sqlOf(tc.pipeline); !strings.Contains(got, tc.want) {
			t.Errorf("%s:\nSQL %s\nwant it to contain %s", tc.pipeline, got, tc.want)
		}
	}
	for _, tc := range []struct{ pipeline, notWant string }{
		{`jq 'select($line_no == "1")'`, "line_no"},                 // a string never equals a number
		{`jq 'select($level == 1)'`, "level = "},                    // nor a number a string
		{`jq '. as $level | select($level == "x")'`, "level = 'x'"}, // $level is the program's own here
	} {
		if got := sqlOf(tc.pipeline); strings.Contains(strings.SplitN(got, "WHERE", 2)[len(strings.SplitN(got, "WHERE", 2))-1], tc.notWant) {
			t.Errorf("%s lowered to a %q predicate:\n%s", tc.pipeline, tc.notWant, got)
		}
	}
}
