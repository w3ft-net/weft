package syslog

import (
	"testing"
	"time"
)

// one parses a single octet-counted frame and fails the test on any
// per-frame error.
func one(t *testing.T, msg string) Frame {
	t.Helper()
	frames, errs := collect(octet(msg), OctetCounting, 0)
	if len(errs) != 0 {
		t.Fatalf("Parse(%q): %v", msg, errs)
	}
	if len(frames) != 1 {
		t.Fatalf("Parse(%q): got %d frames, want 1", msg, len(frames))
	}
	return frames[0]
}

func TestFrame_HeaderTuple(t *testing.T) {
	f := one(t, `<134>1 2026-04-30T12:00:00.5Z host myapp web.1 msg-42 - hello world`)
	if f.Priority != 134 || f.Facility != 16 || f.Severity != 6 {
		t.Errorf("PRI decode = pri %d fac %d sev %d, want 134/16/6", f.Priority, f.Facility, f.Severity)
	}
	if !f.Timestamp.Equal(time.Date(2026, 4, 30, 12, 0, 0, 500_000_000, time.UTC)) {
		t.Errorf("Timestamp = %v", f.Timestamp)
	}
	if f.Hostname != "host" || f.AppName != "myapp" || f.ProcID != "web.1" || f.MsgID != "msg-42" {
		t.Errorf("header = %q/%q/%q/%q", f.Hostname, f.AppName, f.ProcID, f.MsgID)
	}
	if f.StructuredData != nil {
		t.Errorf("StructuredData = %v, want nil for '-'", f.StructuredData)
	}
	if f.Message != "hello world" {
		t.Errorf("Message = %q", f.Message)
	}
}

func TestFrame_NilValuesBecomeZero(t *testing.T) {
	f := one(t, `<13>1 - - - - - - just a message`)
	if f.Hostname != "" || f.AppName != "" || f.ProcID != "" || f.MsgID != "" {
		t.Errorf("NILVALUE header not flattened to \"\": %q/%q/%q/%q", f.Hostname, f.AppName, f.ProcID, f.MsgID)
	}
	if f.Message != "just a message" {
		t.Errorf("Message = %q", f.Message)
	}
}

func TestFrame_TimestampFallback(t *testing.T) {
	before := time.Now().Add(-time.Second)
	f := one(t, `<13>1 - h a p m - line`)
	if f.Timestamp.Before(before) {
		t.Errorf("NILVALUE timestamp should fall back to ~now, got %v", f.Timestamp)
	}
}

func TestFrame_StructuredData(t *testing.T) {
	f := one(t, `<134>1 2026-04-30T12:00:00Z host app proc msgid `+
		`[exampleSDID@32473 iut="3" eventSource="Application"][meta@1 seq="7"] the message`)
	if got, ok := f.SD("exampleSDID@32473", "iut"); !ok || got != "3" {
		t.Errorf(`SD(exampleSDID@32473, iut) = %q, %v`, got, ok)
	}
	if got, _ := f.SD("exampleSDID@32473", "eventSource"); got != "Application" {
		t.Errorf("eventSource = %q", got)
	}
	if got, _ := f.SD("meta@1", "seq"); got != "7" {
		t.Errorf("meta seq = %q", got)
	}
	if f.Message != "the message" {
		t.Errorf("Message = %q", f.Message)
	}
}

func TestFrame_StructuredDataEscapes(t *testing.T) {
	// RFC 5424 §6.3.3: \" \\ \] are the only defined escapes inside a
	// PARAM-VALUE.
	f := one(t, `<1>1 2026-04-30T12:00:00Z h a p m [sd@1 k="a \"quote\" and a \] bracket"] body`)
	got, _ := f.SD("sd@1", "k")
	if want := `a "quote" and a ] bracket`; got != want {
		t.Errorf("unescaped value = %q, want %q", got, want)
	}
	if f.Message != "body" {
		t.Errorf("Message = %q", f.Message)
	}
}

func TestFrame_RenderExampleShape(t *testing.T) {
	// Render's own doc example line. The <0> PRI
	// is almost certainly a doc placeholder but must still parse.
	f := one(t, `<0>1 2021-03-31T16:00:00-08:00 test-service cron-12345 74440 cron-12345 - hello this is a test`)
	if f.Hostname != "test-service" || f.AppName != "cron-12345" || f.ProcID != "74440" {
		t.Errorf("header = %q/%q/%q", f.Hostname, f.AppName, f.ProcID)
	}
	if f.Message != "hello this is a test" {
		t.Errorf("Message = %q", f.Message)
	}
}
