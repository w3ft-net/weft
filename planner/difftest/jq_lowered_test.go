package difftest

import (
	"fmt"
	"testing"
	"time"

	pb "github.com/w3ft-net/weft/proto/weftpb"
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

// TestDifferentialJqLoweredNumbers: a number a lowered jq program prints is
// written as jq writes it (the shortest digits that round-trip the float64,
// 1e+21 and 1e-07 for the far ends, no trailing .0 and no sign on 0 lost),
// whatever way the payload spelled it, alone, in -r, in an array, in an object.
func TestDifferentialJqLoweredNumbers(t *testing.T) {
	var rows []*pb.LogRow
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for i, n := range []string{
		"2.0", "1500.0", "1e3", "1E5", "1.50", "0.1", "0.30000000000000004", "1e-7", "1e-05", "0.0001", "0.00001234",
		"1e21", "1e20", "1e16", "1e15", "123456789012345678901234", "12345678901234567890", "9007199254740993",
		"0", "0.0", "-0.0", "-1.0", "-123.456e2", "5e-324", "1.7976931348623157e308", "1234567.125",
		"100000", "12345678.9", "1.005", "100.5e-2", "4.35", "0.5", "-2.5e-9",
	} {
		rows = append(rows, &pb.LogRow{
			Raw: fmt.Sprintf("row %d", i), TsUnixNs: base.Add(time.Duration(i) * time.Second).UnixNano(),
			Namespace: "acme", Service: "api", Kind: "main", CustomerId: 1,
			PayloadJson: []byte(fmt.Sprintf(`{"n":%s,"s":"x","o":{"deep":%s}}`, n, n)),
		})
	}
	h := newHarness(t, rows, 2)
	for _, p := range []string{
		`jq '.n'`, `jq -r '.n'`, `jq -c '.n'`, `jq '.o.deep'`, `jq '[.n, .s]'`, `jq '[.n, .o.deep]'`,
		`jq '{n}'`, `jq -c '{a: .n, b: .o.deep, c: .s}'`, `jq 'select(.s == "x") | .n'`,
		`jq -r 'select(.n > 1) | .n'`,
	} {
		p := `cat /log/acme/api/main.log | ` + p
		t.Run(p, func(t *testing.T) { h.check(t, p) })
	}
}

// TestDifferentialJqLoweredStrings: a string a lowered jq program prints is
// written as jq writes it: bare when it is plain printable ASCII, else escaped,
// with <, > and & escaped too once the string is escaped at all.
func TestDifferentialJqLoweredStrings(t *testing.T) {
	var rows []*pb.LogRow
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for i, s := range []string{
		`plain text`, `a & b`, `<html>`, `x > y`, `é & <b>`, `quote\" and <tag>`, `back\\slash & more`,
		`naïve café`, `日本語 テスト`, `emoji 😀 & <`, `tab\there`, `new\nline <`, `\u0001 ctrl & <`, `bs\b ff\f`,
		`del\u007f`, `sep para  <`, `slash\/ and é`, ``, ` `, `\"`, `\\`,
	} {
		rows = append(rows, &pb.LogRow{
			Raw: fmt.Sprintf("row %d", i), TsUnixNs: base.Add(time.Duration(i) * time.Second).UnixNano(),
			Namespace: "acme", Service: "api", Kind: "main", CustomerId: 1,
			PayloadJson: []byte(fmt.Sprintf(`{"s":"%s","t":"%s","o":{"in":"%s"}}`, s, s, s)),
		})
	}
	h := newHarness(t, rows, 2)
	for _, p := range []string{
		`jq '.s'`, `jq -r '.s'`, `jq -c '.s'`, `jq '.o.in'`, `jq '[.s, .t]'`, `jq '{s, t}'`,
		`jq -c '{a: .s, b: .o.in}'`, `jq 'select(.s != null) | .s'`,
	} {
		p := `cat /log/acme/api/main.log | ` + p
		t.Run(p, func(t *testing.T) { h.check(t, p) })
	}
}
