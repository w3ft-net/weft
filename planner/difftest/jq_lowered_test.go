package difftest

import (
	"fmt"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// TestDifferentialJqLoweredPayloads: jq programs the planner lowers whole
// (no compute stage) must print what jq prints over the same payloads, and
// nothing for a row jq errors on: a payload that isn't an object, a key read
// through a value of the wrong type.
func TestDifferentialJqLoweredPayloads(t *testing.T) {
	var rows []*pb.LogRow
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for i, payload := range []string{
		`{"a":1.50,"e":"é\u00e9\n\t\"q\"","f":[1,{"g":null}],"h":null}`,
		`{}`,
		`{"a":{"b":{"c":1}},"items":[3,1,2],"status":500,"action":"login"}`,
		`{"status":"500","action":"Logout"}`,
		`{"status":200,"action":"naïve café","duration_ms":12.5}`,
		`{"a":true,"b":false,"c":[],"d":{},"e":"","status":null}`,
		`{"a":"x","b":"y","status":503,"action":"login"}`,
		`{"a":"a string, not an object","status":1}`,
		`[1,2,3]`,
		`"just a string"`,
		`3`,
		`true`,
		`null`,
	} {
		rows = append(rows, &pb.LogRow{
			Raw: fmt.Sprintf("row %d", i), TsUnixNs: base.Add(time.Duration(i) * time.Second).UnixNano(),
			Namespace: "acme", Service: "api", Kind: "main", CustomerId: 1, PayloadJson: []byte(payload),
		})
	}
	h := newHarness(t, rows, 2)
	for _, p := range []string{
		`jq '.a'`, `jq -r '.e'`, `jq '.f'`, `jq '.h'`, `jq '.missing'`, `jq '.a.b.c'`, `jq '.a.b'`,
		`jq '.items[0]'`, `jq '[.a, .e]'`, `jq '{a, e}'`, `jq -c '{s: .status, a: .action}'`,
		`jq '{x: .a.b}'`, `jq '[.a.b.c, .status]'`, `jq '.status'`, `jq -r '.status'`, `jq -r '.action'`,
		`jq 'select(.status == 500)'`, `jq 'select(.status == null)'`, `jq 'select(.a == null)'`,
		`jq 'select(.status != 500)'`, `jq 'select(.action == "login") | .status'`,
		`jq 'select(.status == null) | .action'`, `jq 'select(.a.b == null)'`,
	} {
		p := `cat /log/acme/api/main.log | ` + p
		t.Run(p, func(t *testing.T) { h.check(t, p) })
	}
}
