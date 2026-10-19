package weftd

import (
	"strings"
	"testing"
)

func hdr(vals ...string) string {
	s := "\x1e"
	for i, v := range vals {
		if i > 0 {
			s += "\x1f"
		}
		s += v
	}
	return s + "\x1e"
}

func TestParseHeader_V1AllValues(t *testing.T) {
	line := hdr("v1", "1700000000123456789", "WARN", "abc123", "db.query", "s1", "s0", "2500", "json", "stderr") + `{"msg":"x"}`
	h, body, ok := ParseHeader(line)
	if !ok {
		t.Fatal("ok = false, want a header")
	}
	want := LineHeader{
		TsNs: 1700000000123456789, Level: "WARN", TraceID: "abc123",
		SpanName: "db.query", SpanID: "s1", ParentID: "s0", DurNs: 2500,
		Fmt: "json", Kind: "stderr",
	}
	if h != want {
		t.Errorf("header = %+v, want %+v", h, want)
	}
	if body != `{"msg":"x"}` {
		t.Errorf("body = %q", body)
	}
}

func TestParseHeader_NoHeader(t *testing.T) {
	for _, line := range []string{"", "plain text", "\x1eunclosed", `{"a":1}`, "info: \x1e not at start \x1e"} {
		h, body, ok := ParseHeader(line)
		if ok || body != line || h != (LineHeader{}) {
			t.Errorf("ParseHeader(%q) = %+v, %q, %v; want no header and the line unchanged", line, h, body, ok)
		}
	}
}

// Empty values mean "not supplied"; a short value list is padded, not an error.
func TestParseHeader_EmptyAndShortValues(t *testing.T) {
	h, body, ok := ParseHeader(hdr("v1", "", "", "t1") + "text")
	if !ok || body != "text" {
		t.Fatalf("ok=%v body=%q", ok, body)
	}
	if h.TsNs != 0 || h.Level != "" || h.TraceID != "t1" || h.Fmt != "" || h.Kind != "" {
		t.Errorf("header = %+v, want only the trace ID set", h)
	}
	if h, _, ok := ParseHeader(hdr("v1") + "text"); !ok || h != (LineHeader{}) {
		t.Errorf("version-only header = %+v, %v; want an empty header", h, ok)
	}
}

// A higher version is read as v1: its first v1 values are used and the rest
// ignored, and the body is still found.
func TestParseHeader_HigherVersionAppends(t *testing.T) {
	line := hdr("v7", "5", "INFO", "t", "", "", "", "", "text", "stdout", "future1", "future2") + "body"
	h, body, ok := ParseHeader(line)
	if !ok || body != "body" {
		t.Fatalf("ok=%v body=%q", ok, body)
	}
	if h.TsNs != 5 || h.Level != "INFO" || h.TraceID != "t" || h.Fmt != "text" || h.Kind != "stdout" {
		t.Errorf("header = %+v", h)
	}
}

// The legacy socket envelope \x1eTRACE\x1eline reads as a trace ID only.
func TestParseHeader_LegacyEnvelope(t *testing.T) {
	h, body, ok := ParseHeader("\x1eabc123\x1eraw line")
	if !ok || body != "raw line" {
		t.Fatalf("ok=%v body=%q", ok, body)
	}
	if h != (LineHeader{TraceID: "abc123"}) {
		t.Errorf("header = %+v, want trace ID only", h)
	}
}

// A bad value is treated as not supplied; it never fails the line.
func TestParseHeader_BadValuesDropped(t *testing.T) {
	cases := map[string]LineHeader{
		hdr("v1", "notanumber", "", "", "", "", "", "-4", "yaml", "bad/kind"): {},
		hdr("v1", "", "", "", "", "", "", "", "", "heartbeats"):               {}, // reserved
		hdr("v1", "", "", "", "", "", "", "", "", "host"):                     {},
	}
	for line, want := range cases {
		h, _, ok := ParseHeader(line + "x")
		if !ok || h != want {
			t.Errorf("ParseHeader(%q) = %+v, %v; want %+v", line, h, ok, want)
		}
	}
}

// v0 and a malformed version are not headers.
func TestParseHeader_BadVersion(t *testing.T) {
	for _, line := range []string{hdr("v0", "1", "x") + "b", hdr("vx", "1", "x") + "b", hdr("1", "2") + "b"} {
		if _, body, ok := ParseHeader(line); ok || body != line {
			t.Errorf("ParseHeader(%q): ok=%v body=%q, want no header", line, ok, body)
		}
	}
}

// The file shipper trims each line before building the row; the header's
// control bytes must survive that.
func TestParseHeader_SurvivesTrimSpace(t *testing.T) {
	line := strings.TrimSpace(hdr("v1", "", "", "t") + "body \n")
	if h, body, ok := ParseHeader(line); !ok || h.TraceID != "t" || body != "body" {
		t.Errorf("header lost after TrimSpace: %+v %q %v", h, body, ok)
	}
}
