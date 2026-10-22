package coordinator

import (
	"bytes"
	"strings"
	"testing"
	"time"

	luapkg "github.com/graybeardcorp/warp/pkg/lua"
)

// mockWeftFuncs implements weftQueryFuncs so the production
// LuaExtension can run against synthetic data without a real
// *store.Store.
type mockWeftFuncs struct {
	latestVal map[string]string
	prevVal   map[string]string
	results   map[string][]float64
	seasonal  map[string]SeasonalStatsResult
	runs      map[string][]RunRecord
}

func (m *mockWeftFuncs) Latest(name string) (string, error) {
	if v, ok := m.latestVal[name]; ok {
		return v, nil
	}
	return "", nil
}
func (m *mockWeftFuncs) Previous(name string) (string, error) {
	if v, ok := m.prevVal[name]; ok {
		return v, nil
	}
	return "", nil
}
func (m *mockWeftFuncs) Describe(name string, _ int) ([]RunRecord, error) { return m.runs[name], nil }
func (m *mockWeftFuncs) SeasonalStats(name string, _, _ time.Duration) (SeasonalStatsResult, error) {
	if v, ok := m.seasonal[name]; ok {
		return v, nil
	}
	return SeasonalStatsResult{}, nil
}
func (m *mockWeftFuncs) values(name string) []float64 { return m.results[name] }
func (m *mockWeftFuncs) Avg(name string, _ time.Duration) (float64, error) {
	vals := m.values(name)
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals)), nil
}
func (m *mockWeftFuncs) Max(name string, _ time.Duration) (float64, error) {
	vals := m.values(name)
	max := vals[0]
	for _, v := range vals[1:] {
		if v > max {
			max = v
		}
	}
	return max, nil
}
func (m *mockWeftFuncs) Min(name string, _ time.Duration) (float64, error) {
	vals := m.values(name)
	min := vals[0]
	for _, v := range vals[1:] {
		if v < min {
			min = v
		}
	}
	return min, nil
}
func (m *mockWeftFuncs) Sum(name string, _ time.Duration) (float64, error) {
	vals := m.values(name)
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum, nil
}
func (m *mockWeftFuncs) Stddev(name string, _ time.Duration) (float64, error) {
	vals := m.values(name)
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(len(vals))
	var variance float64
	for _, v := range vals {
		d := v - mean
		variance += d * d
	}
	variance /= float64(len(vals))
	return variance, nil
}
func (m *mockWeftFuncs) First(name string, _ time.Duration) (float64, error) {
	vals := m.values(name)
	return vals[0], nil
}
func (m *mockWeftFuncs) Count(name string, _ time.Duration) (float64, error) {
	return float64(len(m.values(name))), nil
}
func (m *mockWeftFuncs) Absent(name string, window time.Duration) (bool, error) {
	if window > 0 {
		return len(m.values(name)) == 0, nil
	}
	return m.latestVal[name] == "", nil
}
func (m *mockWeftFuncs) Percentile(name string, _ time.Duration, p float64) (float64, error) {
	vals := append([]float64(nil), m.values(name)...)
	if len(vals) == 0 {
		return 0, nil
	}
	for i := 1; i < len(vals); i++ {
		for j := i; j > 0 && vals[j-1] > vals[j]; j-- {
			vals[j-1], vals[j] = vals[j], vals[j-1]
		}
	}
	if len(vals) == 1 {
		return vals[0], nil
	}
	rank := p / 100 * float64(len(vals)-1)
	lo := int(rank)
	frac := rank - float64(lo)
	if lo+1 >= len(vals) {
		return vals[len(vals)-1], nil
	}
	return vals[lo] + frac*(vals[lo+1]-vals[lo]), nil
}
func (m *mockWeftFuncs) Delta(name string, _ time.Duration) (float64, error) {
	vals := m.values(name)
	return vals[len(vals)-1] - vals[0], nil
}
func (m *mockWeftFuncs) Slope(name string, dur time.Duration) (float64, error) {
	vals := m.values(name)
	return (vals[len(vals)-1] - vals[0]) / dur.Seconds(), nil
}
func (m *mockWeftFuncs) Rate(name string, dur time.Duration) (float64, error) {
	vals := m.values(name)
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / dur.Seconds(), nil
}

func newMock() *mockWeftFuncs {
	return &mockWeftFuncs{
		latestVal: map[string]string{
			"cpu-check":   "85",
			"error-count": "42",
			"disk-pct":    "73.5",
		},
		results: map[string][]float64{
			"cpu-check":   {70, 75, 80, 85, 90},
			"error-count": {10, 20, 30, 40, 42},
			"disk-pct":    {70.0, 71.5, 72.0, 73.0, 73.5},
		},
	}
}

func runLuaWithWeftMock(t *testing.T, source, stdin string, m *mockWeftFuncs) (stdout, stderr string, exitCode int) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	code := luapkg.RunScript(source, nil, strings.NewReader(stdin), &outBuf, &errBuf, luapkg.WithExtension(buildWeftLuaExtension(m)))
	return outBuf.String(), errBuf.String(), code
}

// ── weft.latest ─────────────────────────────────────────────

func TestLuaWeftLatest(t *testing.T) {
	out, _, code := runLuaWithWeftMock(t, `print(weft.latest("cpu-check"))`, "", newMock())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "85" {
		t.Errorf("got %q", out)
	}
}

func TestLuaWeftLatestMultiple(t *testing.T) {
	script := `
local cpu = weft.latest("cpu-check")
local errors = weft.latest("error-count")
print(cpu .. " " .. errors)
`
	out, _, code := runLuaWithWeftMock(t, script, "", newMock())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "85 42" {
		t.Errorf("got %q", out)
	}
}

// ── weft.avg / max / min / sum / stddev ─────────────────────

func TestLuaWeftAvg(t *testing.T) {
	out, _, code := runLuaWithWeftMock(t, `print(weft.avg("cpu-check", "1h"))`, "", newMock())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "80" {
		t.Errorf("got %q", out)
	}
}

func TestLuaWeftMax(t *testing.T) {
	out, _, code := runLuaWithWeftMock(t, `print(weft.max("cpu-check", "1h"))`, "", newMock())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "90" {
		t.Errorf("got %q", out)
	}
}

func TestLuaWeftMin(t *testing.T) {
	out, _, code := runLuaWithWeftMock(t, `print(weft.min("error-count", "1h"))`, "", newMock())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "10" {
		t.Errorf("got %q", out)
	}
}

func TestLuaWeftSum(t *testing.T) {
	out, _, code := runLuaWithWeftMock(t, `print(weft.sum("error-count", "1h"))`, "", newMock())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "142" {
		t.Errorf("got %q", out)
	}
}

func TestLuaWeftStddev(t *testing.T) {
	// Mock returns variance for stddev (simplifies the test). cpu-check
	// vals 70,75,80,85,90 → mean 80 → variance 50.
	out, _, code := runLuaWithWeftMock(t, `print(weft.stddev("cpu-check", "1h"))`, "", newMock())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "50" {
		t.Errorf("got %q", out)
	}
}

// ── Combined: weft + lines + json ───────────────────────────

func TestLuaWeftWithJsonAndLines(t *testing.T) {
	script := `
local threshold = tonumber(weft.latest("cpu-check"))
for line in lines() do
  local m = json.decode(line)
  if m and m.value > threshold then
    print(json.encode({alert = m.name, value = m.value}))
  end
end
`
	stdin := `{"name":"api","value":90}
{"name":"web","value":50}
{"name":"worker","value":95}
`
	out, _, code := runLuaWithWeftMock(t, script, stdin, newMock())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 alert lines, got %d: %q", len(lines), out)
	}
	if !strings.Contains(lines[0], "api") || !strings.Contains(lines[1], "worker") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestLuaWeftConditionalAlert(t *testing.T) {
	script := `
local cpu = tonumber(weft.latest("cpu-check"))
local avg_errors = weft.avg("error-count", "1h")
local disk = tonumber(weft.latest("disk-pct"))

if cpu > 80 and avg_errors > 20 then
  io.write("HIGH CPU + ERRORS")
end
if disk > 90 then
  io.write(" DISK CRITICAL")
end
`
	out, _, code := runLuaWithWeftMock(t, script, "", newMock())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out != "HIGH CPU + ERRORS" {
		t.Errorf("got %q", out)
	}
}

func TestLuaWeftAggComparison(t *testing.T) {
	script := `
local avg = weft.avg("cpu-check", "1h")
local max = weft.max("cpu-check", "1h")
local min = weft.min("cpu-check", "1h")
local sum = weft.sum("cpu-check", "1h")
print(string.format("avg=%.0f max=%.0f min=%.0f sum=%.0f", avg, max, min, sum))
`
	out, _, code := runLuaWithWeftMock(t, script, "", newMock())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "avg=80 max=90 min=70 sum=400" {
		t.Errorf("got %q", out)
	}
}

// ── Error handling ──────────────────────────────────────────

func TestLuaWeftInvalidDuration(t *testing.T) {
	script := `
local val, err = weft.avg("cpu-check", "abc")
if err then print("ERROR: " .. err) end
`
	out, _, code := runLuaWithWeftMock(t, script, "", newMock())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "ERROR:") {
		t.Errorf("expected error message, got %q", out)
	}
}

func TestLuaWeftDurationTooLong(t *testing.T) {
	script := `
local val, err = weft.avg("cpu-check", "401d")
if err then print("ERROR: " .. err) end
`
	out, _, code := runLuaWithWeftMock(t, script, "", newMock())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "ERROR:") {
		t.Errorf("expected duration error, got %q", out)
	}
}

// ── Lua + pipeline passthrough ──────────────────────────────

func TestLuaFilterWithWeftThreshold(t *testing.T) {
	script := `
local max_errors = weft.max("error-count", "1h")
for line in lines() do
  local count = tonumber(line)
  if count and count > max_errors then
    print(line)
  end
end
`
	stdin := "10\n20\n50\n100\n30"
	out, _, code := runLuaWithWeftMock(t, script, stdin, newMock())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "50\n100" {
		t.Errorf("got %q", out)
	}
}

func TestLuaWithoutWeftExtension(t *testing.T) {
	// weft table should not exist when no extension is installed.
	var out, errBuf bytes.Buffer
	code := luapkg.RunScript(`print(type(weft))`, nil, strings.NewReader(""), &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out.String()) != "nil" {
		t.Errorf("expected weft to be nil without extension, got %q", out.String())
	}
}

// ── weft predicates ─────────────────────────────────────────

func TestLuaWeftAbove_Latest(t *testing.T) {
	for _, tc := range []struct {
		thr  string
		want string
	}{{"80", "true"}, {"90", "false"}} {
		src := `print(weft.is_above("cpu-check", ` + tc.thr + `))`
		out, _, code := runLuaWithWeftMock(t, src, "", newMock())
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if strings.TrimSpace(out) != tc.want {
			t.Errorf("above %s: got %q want %q", tc.thr, out, tc.want)
		}
	}
}

func TestLuaWeftBelow_Window(t *testing.T) {
	// cpu-check window={70,75,80,85,90}, default form = max < threshold.
	// max=90, so below(91) is true, below(89) is false.
	for _, tc := range []struct {
		thr  string
		want string
	}{{"91", "true"}, {"89", "false"}} {
		src := `print(weft.is_below("cpu-check", ` + tc.thr + `, "1h"))`
		out, _, code := runLuaWithWeftMock(t, src, "", newMock())
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if strings.TrimSpace(out) != tc.want {
			t.Errorf("below %s: got %q want %q", tc.thr, out, tc.want)
		}
	}
}

func TestLuaWeftAbsent_NoLatest(t *testing.T) {
	out, _, code := runLuaWithWeftMock(t, `print(weft.is_absent("never-seen"))`, "", newMock())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "true" {
		t.Errorf("got %q", out)
	}
}

func TestLuaWeftChanged_WithPrev(t *testing.T) {
	m := newMock()
	m.prevVal = map[string]string{"cpu-check": "84"}
	out, _, code := runLuaWithWeftMock(t, `print(weft.has_changed("cpu-check"))`, "", m)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "true" {
		t.Errorf("got %q", out)
	}
}

func TestLuaWeftChanged_SameValue(t *testing.T) {
	m := newMock()
	m.prevVal = map[string]string{"cpu-check": "85"}
	out, _, code := runLuaWithWeftMock(t, `print(weft.has_changed("cpu-check"))`, "", m)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "false" {
		t.Errorf("got %q", out)
	}
}

func TestLuaWeftRising_Pct(t *testing.T) {
	m := newMock()
	m.prevVal = map[string]string{"cpu-check": "80"}
	for _, tc := range []struct {
		pct  string
		want string
	}{{"5", "true"}, {"10", "false"}} {
		src := `print(weft.is_rising("cpu-check", ` + tc.pct + `))`
		out, _, code := runLuaWithWeftMock(t, src, "", m)
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		if strings.TrimSpace(out) != tc.want {
			t.Errorf("rising %s: got %q want %q", tc.pct, out, tc.want)
		}
	}
}

func TestLuaWeftFalling_NoPrev(t *testing.T) {
	out, _, code := runLuaWithWeftMock(t, `print(weft.is_falling("cpu-check"))`, "", newMock())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "false" {
		t.Errorf("got %q", out)
	}
}

func TestLuaWeftCompositeAnd(t *testing.T) {
	// cpu above 80 AND error-count above 40 → both true → composite true.
	script := `print(weft.is_above("cpu-check", 80) and weft.is_above("error-count", 40))`
	out, _, code := runLuaWithWeftMock(t, script, "", newMock())
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "true" {
		t.Errorf("got %q", out)
	}
}
