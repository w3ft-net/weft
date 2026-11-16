package syslog

import (
	"strconv"
	"strings"
	"testing"
)

// collect runs ParseStream over in and returns the good frames and the
// per-frame errors separately.
func collect(in string, framing Framing, maxFrame int) (frames []Frame, errs []error) {
	ParseStream(strings.NewReader(in), framing, maxFrame, func(f Frame, err error) {
		if err != nil {
			errs = append(errs, err)
			return
		}
		frames = append(frames, f)
	})
	return
}

func octet(msg string) string { return strconv.Itoa(len(msg)) + " " + msg }

func TestParseStream_OctetCounting(t *testing.T) {
	body := octet("<134>1 2026-04-30T12:00:00Z h a p m - first") +
		octet("<134>1 2026-04-30T12:00:01Z h a p m - second") +
		octet("<134>1 2026-04-30T12:00:02Z h a p m - third")
	frames, errs := collect(body, OctetCounting, 0)
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(frames))
	}
	if frames[0].Message != "first" || frames[2].Message != "third" {
		t.Errorf("messages = %q / %q", frames[0].Message, frames[2].Message)
	}
}

func TestParseStream_OctetCountingEmbeddedNewline(t *testing.T) {
	// The length is a byte count, so an embedded newline in the message
	// is not a frame boundary.
	msg := "<1>1 2026-04-30T12:00:00Z h a p m - line one\nline two"
	frames, errs := collect(octet(msg)+octet("<1>1 2026-04-30T12:00:01Z h a p m - next"), OctetCounting, 0)
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if len(frames) != 2 || frames[0].Message != "line one\nline two" {
		t.Fatalf("frames = %+v", frames)
	}
}

func TestParseStream_MalformedFrameSkipped(t *testing.T) {
	// Valid framing, junk contents in the middle — the good frames on
	// either side still come through. This is the Heroku drain contract.
	body := octet("<134>1 2026-04-30T12:00:00Z h a p m - good one") +
		octet("not actually rfc5424") +
		octet("<134>1 2026-04-30T12:00:02Z h a p m - good two")
	frames, errs := collect(body, OctetCounting, 0)
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2 (%+v)", len(frames), frames)
	}
	if len(errs) != 1 {
		t.Errorf("errs = %d, want 1 for the junk frame", len(errs))
	}
}

func TestParseStream_LFDelimited(t *testing.T) {
	body := "<134>1 2026-04-30T12:00:00Z h a p m - first\n<134>1 2026-04-30T12:00:01Z h a p m - second\n"
	frames, errs := collect(body, NonTransparent, 0)
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
}

func TestParseStream_Detect(t *testing.T) {
	octetBody := octet("<1>1 2026-04-30T12:00:00Z h a p m - via octet")
	lfBody := "<1>1 2026-04-30T12:00:00Z h a p m - via lf\n"

	if frames, _ := collect(octetBody, Detect, 0); len(frames) != 1 || frames[0].Message != "via octet" {
		t.Errorf("octet autodetect: frames = %+v", frames)
	}
	if frames, _ := collect(lfBody, Detect, 0); len(frames) != 1 || frames[0].Message != "via lf" {
		t.Errorf("lf autodetect: frames = %+v", frames)
	}
}

func TestParseStream_EmptyStream(t *testing.T) {
	frames, errs := collect("", Detect, 0)
	if len(frames) != 0 || len(errs) != 0 {
		t.Fatalf("frames = %+v, errs = %v", frames, errs)
	}
}

func TestParseStream_OversizeFrame(t *testing.T) {
	huge := "<1>1 2026-04-30T12:00:00Z h a p m - " + strings.Repeat("A", 4000)
	_, errs := collect(octet(huge), OctetCounting, 1<<10)
	if len(errs) == 0 {
		t.Fatal("expected an error for a frame exceeding maxFrame")
	}
}
