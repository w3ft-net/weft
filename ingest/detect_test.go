package ingest

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func readSampleLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Split(string(data), "\n")
}

func TestDetect_GrantCentralWeb(t *testing.T) {
	lines := readSampleLines(t, "../sample-logs/grantcentral_web_logs.txt")
	det := Detect(lines)

	// These are access logs behind a constant "[time] [id]" prefix, which needs
	// no special handling: the prefix is just fields.
	// The timestamp is in CLF format inside brackets: [28/Mar/2026:16:44:40 +0000]
	if len(det.TimestampCandidates) == 0 {
		t.Error("expected at least one timestamp candidate")
	} else {
		t.Logf("timestamp candidates:")
		for _, tc := range det.TimestampCandidates {
			t.Logf("  field %d (spans %d): %s  format=%s", tc.FieldIndex, tc.SpanFields, tc.Sample, tc.Format)
		}
	}

	// CLF doesn't have an explicit level field.
	t.Logf("level candidates: %d", len(det.LevelCandidates))
	for _, lc := range det.LevelCandidates {
		t.Logf("  field %d: %s (pattern=%s)", lc.FieldIndex, lc.Sample, lc.Pattern)
	}

	if det.SuggestFormat() != "text" {
		t.Errorf("expected text format, got %s", det.SuggestFormat())
	}
}

func TestDetect_GrantCentralWorker(t *testing.T) {
	lines := readSampleLines(t, "../sample-logs/grantcentral_worker_logs.txt")
	det := Detect(lines)

	// Behind a constant prefix, the worker's lines look like:
	// [2026-03-22 12:39:02,057: INFO/MainProcess] celery@ip-172-31-44-106 ready.
	// Should detect a timestamp candidate and a level candidate.
	if len(det.TimestampCandidates) == 0 {
		t.Error("expected at least one timestamp candidate")
	} else {
		t.Logf("timestamp candidates:")
		for _, tc := range det.TimestampCandidates {
			t.Logf("  field %d (spans %d): %s  format=%s", tc.FieldIndex, tc.SpanFields, tc.Sample, tc.Format)
		}
	}

	if len(det.LevelCandidates) == 0 {
		t.Error("expected at least one level candidate (INFO/ERROR in Celery format)")
	} else {
		t.Logf("level candidates:")
		for _, lc := range det.LevelCandidates {
			t.Logf("  field %d: %s (pattern=%s)", lc.FieldIndex, lc.Sample, lc.Pattern)
		}
	}
}

func TestDetect_VimGym(t *testing.T) {
	lines := readSampleLines(t, "../sample-logs/vimgym_logs.txt")
	det := Detect(lines)

	// VimGym Go logs: "2026/03/24 17:21:21 bot: created ..."
	// Should detect Go timestamp format.
	if len(det.TimestampCandidates) == 0 {
		t.Error("expected at least one timestamp candidate")
	} else {
		t.Logf("timestamp candidates:")
		for _, tc := range det.TimestampCandidates {
			t.Logf("  field %d (spans %d): %s  format=%s", tc.FieldIndex, tc.SpanFields, tc.Sample, tc.Format)
		}
	}

	// Go logs don't have explicit level fields in most lines.
	t.Logf("level candidates: %d", len(det.LevelCandidates))
	t.Logf("json coverage: %.1f%%, json tail coverage: %.1f%%",
		det.JSONCoverage*100, det.JSONTailCoverage*100)
	t.Logf("suggested format: %s", det.SuggestFormat())
}

func TestDetect_JSONLines(t *testing.T) {
	lines := []string{
		`{"ts": "2026-03-28T10:00:00Z", "level": "INFO", "msg": "started"}`,
		`{"ts": "2026-03-28T10:00:01Z", "level": "ERROR", "msg": "failed"}`,
		`{"ts": "2026-03-28T10:00:02Z", "level": "INFO", "msg": "recovered"}`,
	}
	det := Detect(lines)

	if det.JSONCoverage < 0.9 {
		t.Errorf("expected high JSON coverage, got %.1f%%", det.JSONCoverage*100)
	}
	if det.SuggestFormat() != "json" {
		t.Errorf("expected json format, got %s", det.SuggestFormat())
	}
}

func TestDetect_PrefixJSON(t *testing.T) {
	lines := []string{
		`2026-03-28 10:00:00 INFO {"user_id": "u1", "action": "login"}`,
		`2026-03-28 10:00:01 ERROR {"user_id": "u2", "action": "signup", "error": "timeout"}`,
		`2026-03-28 10:00:02 INFO {"user_id": "u3", "action": "list"}`,
	}
	det := Detect(lines)

	if det.JSONTailCoverage < 0.9 {
		t.Errorf("expected high JSON tail coverage, got %.1f%%", det.JSONTailCoverage*100)
	}
	if det.JSONTailStartField == 0 {
		t.Error("expected non-zero JSON tail start field")
	}
	t.Logf("JSON tail starts at field %d", det.JSONTailStartField)
}

// A bracketed timestamp with a trailing colon ("[2026-10-15 12:00:01,123:")
// is found, and at the field it starts in. The two passes that find the
// timestamp and then the field used different trims, so the field came back
// as 0 ("none") and the candidate extracted nothing.
func TestDetectTimestampBracketedWithTrailingColon(t *testing.T) {
	lines := []string{
		"[2026-10-15 12:00:01,123: INFO/MainProcess] Task a.b[1] received",
		"[2026-10-15 12:00:02,456: INFO/MainProcess] Task a.b[1] succeeded",
		"[2026-10-15 12:00:03,789: WARNING/ForkPoolWorker-1] Task a.b[2] slow",
		"[2026-10-15 12:00:04,012: INFO/MainProcess] Task a.b[3] received",
	}
	d := Detect(lines)
	var found *TimestampCandidate
	for i := range d.TimestampCandidates {
		if d.TimestampCandidates[i].Format == "2006-01-02 15:04:05,000" {
			found = &d.TimestampCandidates[i]
		}
	}
	if found == nil {
		t.Fatalf("no comma-millis candidate among %+v", d.TimestampCandidates)
	}
	if found.FieldIndex != 1 || found.SpanFields != 2 {
		t.Errorf("candidate = %+v, want it to start at field 1 and span 2", *found)
	}
}

// repeat returns the given lines cycled to n lines, so a fixture looks like a
// sample (levels recur) rather than a handful of distinct lines.
func repeat(lines []string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = lines[i%len(lines)]
	}
	return out
}

// A level field is found by structure alone: a field whose leading word comes
// from a small set that keeps recurring, then an optional delimiter. No list
// of level words exists to consult, so words no list would hold are found
// like any other, and what follows the delimiter is irrelevant.
func TestDetectCategoricalFieldByPositionAndCut(t *testing.T) {
	for name, c := range map[string]struct {
		lines  []string
		field  int
		cutAt  string
		values []string
	}{
		"hyphenated suffix after a delimiter": {repeat([]string{
			"[2026-10-15 12:00:01,123: INFO/MainProcess] a",
			"[2026-10-15 12:00:02,456: WARNING/ForkPoolWorker-1] b",
			"[2026-10-15 12:00:03,789: ERROR/ForkPoolWorker-12] c",
			"[2026-10-15 12:00:04,012: INFO/MainProcess] d",
		}, 12), 3, "/", []string{"ERROR", "INFO", "WARNING"}},
		"words no level list would hold": {repeat([]string{
			"2026-10-15 12:00:01 SEVERE db down",
			"2026-10-15 12:00:02 NOTICE reload",
			"2026-10-15 12:00:03 ALERT disk",
			"2026-10-15 12:00:04 NOTICE reload",
		}, 12), 3, "", []string{"ALERT", "NOTICE", "SEVERE"}},
		"bracketed": {repeat([]string{
			"2026-10-15 12:00:01 [INFO] started",
			"2026-10-15 12:00:02 [WARN] slow",
			"2026-10-15 12:00:03 [ERROR] failed",
			"2026-10-15 12:00:04 [INFO] ok",
		}, 12), 3, "", []string{"ERROR", "INFO", "WARN"}},
		"trailing colon is frame, not a cut": {repeat([]string{
			"2026-10-15 12:00:01 INFO: started",
			"2026-10-15 12:00:02 WARN: slow",
			"2026-10-15 12:00:03 INFO: ok",
		}, 12), 3, "", []string{"INFO", "WARN"}},
		"a number after a delimiter": {repeat([]string{
			"2026-10-15 12:00:01 INFO|4123 started",
			"2026-10-15 12:00:02 WARN|4123 slow",
			"2026-10-15 12:00:03 INFO|4124 ok",
			"2026-10-15 12:00:04 ERROR|4124 failed",
		}, 12), 3, "|", []string{"ERROR", "INFO", "WARN"}},
	} {
		d := Detect(c.lines)
		var got *FieldCandidate
		for i := range d.LevelCandidates {
			if d.LevelCandidates[i].FieldIndex == c.field {
				got = &d.LevelCandidates[i]
			}
		}
		if got == nil {
			t.Errorf("%s: no candidate at field %d among %+v", name, c.field, d.LevelCandidates)
			continue
		}
		if got.CutAt != c.cutAt || got.Pattern != "" || !reflect.DeepEqual(got.Values, c.values) {
			t.Errorf("%s: candidate = %+v, want cut %q, no pattern, values %q", name, *got, c.cutAt, c.values)
		}
	}
}

// What is not a recurring small set is not offered: a constant field, free
// text, and numbers.
func TestDetectCategoricalIgnoresWhatDoesNotRecur(t *testing.T) {
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "2026-10-15 12:00:00 myservice "+[]string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet", "kilo", "lima"}[i%12]+
			" "+string(rune('a'+i))+"word 200")
	}
	d := Detect(lines)
	for _, c := range d.LevelCandidates {
		t.Errorf("offered field %d (%q): a constant, a high-cardinality word, or a number is not categorical", c.FieldIndex, c.Values)
	}
}

// Candidates come left to right, so a person can be asked about each in turn.
func TestDetectCategoricalCandidatesAreOrderedByPosition(t *testing.T) {
	levels := []string{"INFO", "WARN", "INFO", "ERROR"}
	methods := []string{"GET", "POST", "GET", "POST"}
	var lines []string
	for i := 0; i < 24; i++ {
		lines = append(lines, fmt.Sprintf("2026-10-15 12:00:%02d %s %s /path%d", i, levels[i%4], methods[i%4], i))
	}
	d := Detect(lines)
	var fields []int
	for _, c := range d.LevelCandidates {
		fields = append(fields, c.FieldIndex)
	}
	if !reflect.DeepEqual(fields, []int{3, 4}) {
		t.Errorf("candidate fields = %v, want [3 4] (the level, then the method; the varying path is not offered)", fields)
	}
}

// Timestamps are found by inferring a layout from the data, wherever they
// sit in the line and whatever surrounds them, and the text around them is
// reported as the frame to strip, not baked into the layout.
func TestDetectTimestampsFromTheData(t *testing.T) {
	for name, c := range map[string]struct {
		lines  []string
		field  int
		span   int
		format string
		prefix string
		suffix string
	}{
		"bracketed with a trailing colon": {[]string{
			"[2026-10-15 12:00:01,123: INFO/MainProcess] a",
			"[2026-10-15 12:00:02,456: INFO/MainProcess] b",
			"[2026-10-15 12:00:03,789: WARNING/ForkPoolWorker-1] c",
			"[2026-10-15 12:00:04,012: INFO/MainProcess] d",
		}, 1, 2, "2006-01-02 15:04:05,000", "[", ":"},
		"after other fields": {[]string{
			"host1 app 2026-10-15T12:00:01Z started",
			"host1 app 2026-10-15T12:00:02Z running",
			"host1 app 2026-10-15T12:00:03Z done",
			"host1 app 2026-10-15T12:00:04Z exit",
		}, 3, 1, "2006-01-02T15:04:05Z07:00", "", ""},
		"in brackets with a zone": {[]string{
			`10.0.0.1 - - [15/Oct/2026:12:00:01 +0000] "GET / HTTP/1.1" 200`,
			`10.0.0.2 - - [15/Oct/2026:12:00:02 +0000] "GET /a HTTP/1.1" 200`,
			`10.0.0.1 - - [15/Oct/2026:12:00:03 +0000] "GET /b HTTP/1.1" 404`,
			`10.0.0.3 - - [15/Oct/2026:12:00:04 +0000] "GET /c HTTP/1.1" 200`,
		}, 4, 2, "02/Jan/2006:15:04:05 -0700", "[", "]"},
	} {
		d := Detect(c.lines)
		if len(d.TimestampCandidates) == 0 {
			t.Errorf("%s: no timestamp candidate", name)
			continue
		}
		got := d.TimestampCandidates[0]
		if got.FieldIndex != c.field || got.SpanFields != c.span || got.Format != c.format || got.Prefix != c.prefix || got.Suffix != c.suffix {
			t.Errorf("%s:\n  got  %+v\n  want field %d span %d format %q frame %q..%q", name, got, c.field, c.span, c.format, c.prefix, c.suffix)
		}
	}
}

// A few lines that are not timestamped (a continuation, a stack frame) do not
// stop the rest being recognized.
func TestDetectTimestampsToleratesContinuationLines(t *testing.T) {
	lines := []string{
		"2026-10-15 12:00:01 ERROR boom",
		"    at pkg.Class.method(Class.java:12)",
		"2026-10-15 12:00:02 INFO ok",
		"2026-10-15 12:00:03 INFO ok",
		"2026-10-15 12:00:04 INFO ok",
		"2026-10-15 12:00:05 INFO ok",
	}
	d := Detect(lines)
	if len(d.TimestampCandidates) == 0 || d.TimestampCandidates[0].FieldIndex != 1 || d.TimestampCandidates[0].SpanFields != 2 {
		t.Errorf("candidates = %+v", d.TimestampCandidates)
	}
}

func TestDetectTimestampsNoneWhenAbsent(t *testing.T) {
	lines := []string{"hello world", "starting up now", "all good here", "done"}
	if d := Detect(lines); len(d.TimestampCandidates) != 0 {
		t.Errorf("found timestamps in text without any: %+v", d.TimestampCandidates)
	}
	// A time without a year cannot place a row in time, so it is not offered
	// (the row keeps its ingest time).
	yearless := []string{"Oct 15 12:00:01 a", "Oct 15 12:00:02 b", "Oct 15 12:00:03 c", "Oct 15 12:00:04 d"}
	if d := Detect(yearless); len(d.TimestampCandidates) != 0 {
		t.Errorf("offered a timestamp with no year: %+v", d.TimestampCandidates)
	}
}
