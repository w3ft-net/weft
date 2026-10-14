package compute

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/warp/pkg/jq"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// A jq stage's meta-variables ride in an envelope around the row's
// payload (one jq process handles the whole stream, so there's no
// per-row side channel), and JqPerInput takes it off, binding them.
func TestJqPerInput_UnwrapsPayloadAndBindsEveryAlias(t *testing.T) {
	in, err := jq.ParseValue(WrapJqInput(`{"pct":42}`, map[string]string{"instance": "web-1", "duration_ns": "1500"}))
	if err != nil {
		t.Fatal(err)
	}
	got, vars := JqPerInput(in)
	if got.ToJSON() != `{"pct":42}` {
		t.Errorf("payload = %s, want it as it was", got.ToJSON())
	}
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

// The payload reaches the program untouched: a key named like a
// meta-variable is just a key, and `keys` shows no spliced extras.
func TestJqPerInput_PayloadKeysAreNotMetaVariables(t *testing.T) {
	in, _ := jq.ParseValue(WrapJqInput(`{"level":"payload","keys":1}`, map[string]string{"level": "ERROR"}))
	got, vars := JqPerInput(in)
	if got.ObjGet("level").Str != "payload" || vars["level"].Str != "ERROR" || len(got.ObjMap()) != 2 {
		t.Errorf("payload %s, $level %v", got.ToJSON(), vars["level"])
	}
}

// Input a later jq stage reads, which WrapJqInput didn't make, passes
// through with nothing bound.
func TestJqPerInput_OtherInputPassesThrough(t *testing.T) {
	for _, raw := range []string{`{"a":1}`, `[1]`, `"s"`, `null`, `{"__weft_vars__":1}`} {
		in, _ := jq.ParseValue(raw)
		got, vars := JqPerInput(in)
		if got.ToJSON() != in.ToJSON() || vars != nil {
			t.Errorf("JqPerInput(%s) = %s, %v", raw, got.ToJSON(), vars)
		}
	}
}

// A row with nothing for jq to read stays nothing, so the stream
// doesn't gain a value (or an error) for it.
func TestWrapJqInput_EmptyPayloadStaysEmpty(t *testing.T) {
	for _, p := range []string{"", "  "} {
		if got := WrapJqInput(p, map[string]string{"level": "x"}); got != p {
			t.Errorf("WrapJqInput(%q) = %q", p, got)
		}
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
	vars := awkVarsFromMeta(map[string]string{"instance": "web-1"})
	if vars["HOST"] != "web-1" || vars["INSTANCE"] != "web-1" {
		t.Errorf("awkVarsFromMeta = %v, want both HOST and INSTANCE set to web-1", vars)
	}
}

func TestAwkVarsFromMeta_EmptyIsNil(t *testing.T) {
	if got := awkVarsFromMeta(nil); got != nil {
		t.Errorf("awkVarsFromMeta(nil) = %v, want nil", got)
	}
}

// A one-line event is line 0 of an event with no id, and both are always
// set: awk keeps the last record's value for a name a record doesn't set.
func TestRowMetaVars_EventColumns(t *testing.T) {
	one := rowMetaVars(&pb.ResultRow{Raw: "x"})
	if one["event_id"] != "" || one["line_no"] != "0" {
		t.Errorf("one-line event: event_id=%q line_no=%q, want \"\" and \"0\"", one["event_id"], one["line_no"])
	}
	many := rowMetaVars(&pb.ResultRow{Raw: "x", EventId: "01a13b66-0264-73c0-9c0d-d7fd05050994", LineNo: 3})
	if many["event_id"] != "01a13b66-0264-73c0-9c0d-d7fd05050994" || many["line_no"] != "3" {
		t.Errorf("line 3: event_id=%q line_no=%q", many["event_id"], many["line_no"])
	}
	vars := awkVarsFromMeta(many)
	if vars["EVENT_ID"] != many["event_id"] || vars["LINE_NO"] != "3" {
		t.Errorf("awk vars: %v", vars)
	}
	in, _ := jq.ParseValue(WrapJqInput(`{"a":1}`, many))
	if _, jv := JqPerInput(in); jv["line_no"].Num != 3 || jv["line_no"].Type != jq.TypeNumber || !strings.HasPrefix(jv["event_id"].Str, "01a13b66") {
		t.Errorf("jq vars: %v", jv)
	}
}
