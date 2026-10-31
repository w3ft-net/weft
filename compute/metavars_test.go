package compute

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/warp/pkg/jq"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// allMeta is a stage that may read any meta-variable.
var (
	allMeta   = planner.MetaRead{All: true}
	allJq     = NewJqBinder(allMeta)
	allBinder = newAwkBinder(allMeta)
)

// A jq stage's meta-variables are bound for each input as jq variables, typed:
// the numeric columns as numbers, a column the row lacks as null.
func TestJqBinder_BindsEveryAliasWithItsType(t *testing.T) {
	vars := allJq.Vars(map[string]string{"instance": "web-1", "duration_ns": "1500"})
	// "host" and "instance" alias the instance column.
	if vars["host"].Str != "web-1" || vars["instance"].Str != "web-1" {
		t.Errorf("host/instance = %v/%v, want web-1", vars["host"], vars["instance"])
	}
	if vars["duration_ns"].Type != jq.TypeNumber || vars["duration_ns"].Num != 1500 {
		t.Errorf("duration_ns = %v, want the number 1500", vars["duration_ns"])
	}
	if vars["pod"].Type != jq.TypeNull {
		t.Errorf("pod = %v, want null for a column the row lacks", vars["pod"])
	}
}

// The payload reaches the program untouched: a key named like a meta-variable
// is just a key, and `keys` shows no extras.
func TestJob_JqPayloadKeysAreNotMetaVariables(t *testing.T) {
	job := (&Engine{}).NewJob([]string{`jq -c '[.level, $level, keys]'`}, 1, "ts", 0, false, false)
	job.SendRow(&pb.ResultRow{TsUnixNs: 1, Raw: `{"level":"payload","keys":1}`, MetaVars: map[string]string{"level": "ERROR"}}, "a")
	job.SendSentinel("a")
	out, err := job.Result()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != `["payload","ERROR",["keys","level"]]` {
		t.Errorf("got %s", got)
	}
}

func TestFirstStageIsAwk(t *testing.T) {
	cases := []struct {
		stages []string
		want   bool
	}{
		{nil, false},
		{[]string{"grep ERROR", "sort"}, false},
		{[]string{"awk"}, true},
		{[]string{`awk 'HOST == "web-1"'`, "sort"}, true},
		{[]string{"grep ERROR", `awk 'HOST == "web-1"'`}, false},
	}
	for _, tt := range cases {
		if got := firstStageIsAwk(tt.stages); got != tt.want {
			t.Errorf("firstStageIsAwk(%v) = %v, want %v", tt.stages, got, tt.want)
		}
	}
}

func TestAwkVarsFromMeta(t *testing.T) {
	vars := allBinder.vars(map[string]string{"instance": "web-1"})
	if vars["HOST"] != "web-1" || vars["INSTANCE"] != "web-1" {
		t.Errorf("awkVarsFromMeta = %v, want both HOST and INSTANCE set to web-1", vars)
	}
}

func TestAwkVarsFromMeta_EmptyIsNil(t *testing.T) {
	if got := allBinder.vars(nil); got != nil {
		t.Errorf("allBinder.vars(nil) = %v, want nil", got)
	}
}

// A one-line event is line 0 of an event with no id, and both are always
// set: awk keeps the last record's value for a name a record doesn't set.
func TestRowMetaVars_EventColumns(t *testing.T) {
	one := rowMetaVars(&pb.ResultRow{Raw: "x"}, allMeta)
	if one["event_id"] != "" || one["line_no"] != "0" {
		t.Errorf("one-line event: event_id=%q line_no=%q, want \"\" and \"0\"", one["event_id"], one["line_no"])
	}
	many := rowMetaVars(&pb.ResultRow{Raw: "x", EventId: "01a13b66-0264-73c0-9c0d-d7fd05050994", LineNo: 3}, allMeta)
	if many["event_id"] != "01a13b66-0264-73c0-9c0d-d7fd05050994" || many["line_no"] != "3" {
		t.Errorf("line 3: event_id=%q line_no=%q", many["event_id"], many["line_no"])
	}
	vars := allBinder.vars(many)
	if vars["EVENT_ID"] != many["event_id"] || vars["LINE_NO"] != "3" {
		t.Errorf("awk vars: %v", vars)
	}
	if jv := allJq.Vars(many); jv["line_no"].Num != 3 || jv["line_no"].Type != jq.TypeNumber || !strings.HasPrefix(jv["event_id"].Str, "01a13b66") {
		t.Errorf("jq vars: %v", jv)
	}
}
