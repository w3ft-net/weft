package ingest

import (
	"os"
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

	if !det.HasKevconsEnvelope {
		t.Error("expected kevcons envelope detection")
	}

	if len(det.Instances) == 0 {
		t.Error("expected at least one instance ID")
	}

	// After stripping the kevcons envelope, these are CLF access logs.
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

	if !det.HasKevconsEnvelope {
		t.Error("expected kevcons envelope detection")
	}

	// After stripping, Celery logs look like:
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

	if !det.HasKevconsEnvelope {
		t.Error("expected kevcons envelope detection")
	}

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

func TestDetect_StripEnvelope(t *testing.T) {
	lines := []string{
		"━━━ /aws/test/web ━━━",
		"[2026-03-28 12:00:00] [i-abc123] hello world",
		"[2026-03-28 12:00:01] [i-abc123] goodbye world",
	}
	det := Detect(lines)

	if !det.HasKevconsEnvelope {
		t.Error("expected envelope detected")
	}
	if len(det.StrippedLines) != 2 {
		t.Errorf("expected 2 stripped lines, got %d", len(det.StrippedLines))
	}
	if det.StrippedLines[0] != "hello world" {
		t.Errorf("expected 'hello world', got %q", det.StrippedLines[0])
	}
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
