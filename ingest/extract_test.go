package ingest

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Unix(0, 1)

func mustParser(t *testing.T, s *Schema) *Parser {
	t.Helper()
	p, err := NewParser(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A value with an extract spec reads its fields and strips exactly what the
// schema says; nothing about "[" or ":" is built into the parser for it.
func TestExtractExplicitTrims(t *testing.T) {
	p := mustParser(t, &Schema{
		Format: "text", TimestampField: "1", TimestampFields: 2, TimestampFormat: "2006-01-02 15:04:05,000",
		Extract: map[string]ExtractSpec{"timestamp": {Prefix: "[", Suffix: ":"}},
	})
	row, spans, err := p.ParseLineSpans("[2026-10-15 12:00:03,425: INFO/MainProcess] hi", t0)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 15, 12, 0, 3, 425e6, time.UTC).UnixNano(); row.TsUnixNs != want {
		t.Errorf("ts = %d, want %d", row.TsUnixNs, want)
	}
	if got := spans.Text("[2026-10-15 12:00:03,425: INFO/MainProcess] hi", "timestamp"); got != "2026-10-15 12:00:03,425" {
		t.Errorf("highlight = %q, want the timestamp without bracket or colon", got)
	}

	// Without the spec the same text is stripped only by the legacy defaults,
	// and with an explicit empty spec nothing is stripped at all.
	bare := mustParser(t, &Schema{
		Format: "text", TimestampField: "1", TimestampFields: 2, TimestampFormat: "2006-01-02 15:04:05,000",
		Extract: map[string]ExtractSpec{"timestamp": {}},
	})
	row, _ = bare.ParseLine("[2026-10-15 12:00:03,425: INFO] hi", t0)
	if row.TsUnixNs != t0.UnixNano() {
		t.Errorf("an explicit empty spec must not apply legacy trims, yet the timestamp parsed: %d", row.TsUnixNs)
	}
}

// Negative indexes count from the end of the line: a trailing key=value tag
// is "the last field" however long the message before it is.
func TestExtractNegativeIndex(t *testing.T) {
	p := mustParser(t, &Schema{
		Format: "text", TraceIDField: "-1",
		Extract: map[string]ExtractSpec{"trace_id": {Prefix: "trace_id="}},
	})
	for _, line := range []string{
		"a b trace_id=abc123",
		"a much longer message with many more words in it trace_id=def456",
	} {
		row, _, _ := p.ParseLineSpans(line, t0)
		want := line[strings.LastIndex(line, "=")+1:]
		if row.TraceId != want {
			t.Errorf("%q: trace = %q, want %q", line, row.TraceId, want)
		}
	}
	// A field without the prefix has no value: the tag is not on this line.
	if row, _ := p.ParseLine("one", t0); row.TraceId != "" {
		t.Errorf("a field without the required prefix must read nothing, got %q", row.TraceId)
	}
}

// cut_at keeps the text before a delimiter: INFO/MainProcess and
// WARNING/ForkPoolWorker-1 are both a level and a worker.
func TestExtractCutAt(t *testing.T) {
	p := mustParser(t, &Schema{
		Format: "text", LevelField: "3",
		Extract: map[string]ExtractSpec{"level": {Suffix: "]", CutAt: "/"}},
	})
	for line, want := range map[string]string{
		"[2026-10-15 12:00:01,123: INFO/MainProcess] a":         "INFO",
		"[2026-10-15 12:00:02,456: WARNING/ForkPoolWorker-1] b": "WARNING",
		"[2026-10-15 12:00:03,789: error] c":                    "ERROR", // no delimiter: whole field, uppercased
	} {
		if row, _ := p.ParseLine(line, t0); row.Level != want {
			t.Errorf("%q: level = %q, want %q", line, row.Level, want)
		}
	}
}

// The fallback regex runs only when the positional spec finds nothing.
func TestExtractFallbackOnlyWhenPrimaryEmpty(t *testing.T) {
	p := mustParser(t, &Schema{
		Format: "text", TraceIDField: "3",
		Extract: map[string]ExtractSpec{"trace_id": {Prefix: "id=", Fallback: `(?i)ID:\s*(\w+)`}},
	})
	for line, want := range map[string]string{
		"a b id=abc1 z": "abc1", // field 3 present: the primary read wins
		"ID: ef56":      "ef56", // only two fields: no field 3, so the fallback finds it
		"a b":           "",     // neither finds anything
	} {
		if row, _ := p.ParseLine(line, t0); row.TraceId != want {
			t.Errorf("%q: trace = %q, want %q", line, row.TraceId, want)
		}
	}
}

// A timestamp spanning two fields tolerates irregular whitespace between
// them: the layout has a single space.
func TestExtractTimestampWithIrregularWhitespace(t *testing.T) {
	p := mustParser(t, &Schema{
		Format: "text", TimestampField: "1", TimestampFields: 2, TimestampFormat: "2006-01-02 15:04:05",
		Extract: map[string]ExtractSpec{"timestamp": {}},
	})
	for _, line := range []string{"2026-10-15 12:00:00 x", "2026-10-15   12:00:00 x", "2026-10-15\t12:00:00 x"} {
		if row, _ := p.ParseLine(line, t0); row.TsUnixNs == t0.UnixNano() {
			t.Errorf("%q did not parse", line)
		}
	}
}

func TestExtractCustomFieldSeparator(t *testing.T) {
	p := mustParser(t, &Schema{
		Format: "text", FieldSeparator: "|", LevelField: "2",
		Extract: map[string]ExtractSpec{"level": {}},
	})
	if row, _ := p.ParseLine("2026-10-15|warn|a message with | in it", t0); row.Level != "WARN" {
		t.Errorf("level = %q", row.Level)
	}
}

// The span is relative to the raw line even when an envelope was stripped.
func TestExtractSpansIncludeEnvelopeOffset(t *testing.T) {
	line := "[2026-10-15 12:00:00] [i-0abc12] INFO started"
	p := mustParser(t, &Schema{
		Format: "text", Envelope: "kevcons", LevelField: "1",
		Extract: map[string]ExtractSpec{"level": {}},
	})
	row, spans, _ := p.ParseLineSpans(line, t0)
	if row.Level != "INFO" || spans.Text(line, "level") != "INFO" {
		t.Errorf("level=%q highlight=%q", row.Level, spans.Text(line, "level"))
	}
}

// Stopping the split early must not change any result: compare a schema's
// output against the same schema forced to split the whole line.
func TestExtractEarlySplitEqualsFullSplit(t *testing.T) {
	s := &Schema{
		Format: "text", TimestampField: "1", TimestampFields: 2, TimestampFormat: "2006-01-02 15:04:05.000", LevelField: "3",
		Extract: map[string]ExtractSpec{"timestamp": {}, "level": {}},
	}
	early, full := mustParser(t, s), mustParser(t, s)
	full.forceFullSplit = true
	line := longMessage
	a, _ := early.ParseLine(line, t0)
	b, _ := full.ParseLine(line, t0)
	if a.TsUnixNs != b.TsUnixNs || a.Level != b.Level || a.Level != "INFO" {
		t.Errorf("early %+v vs full %+v", a, b)
	}
}

func TestSplitOffsetsMatchesStringsFields(t *testing.T) {
	for _, in := range []string{
		"", "   ", "a", " a b  c ", "a\tb\nc", "héllo wörld  ok", "tab\tsep  and nbsp", longMessage,
	} {
		var got []string
		for _, o := range splitOffsets(in, nil, 0) {
			got = append(got, in[o[0]:o[1]])
		}
		want := strings.Fields(in)
		if len(want) == 0 {
			want = nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestValidateExtractSpecs(t *testing.T) {
	ok := &Schema{Format: "text", TraceIDField: "-1", Extract: map[string]ExtractSpec{"trace_id": {Prefix: "trace_id="}}}
	if p := ok.Validate(); len(p) != 0 {
		t.Errorf("valid schema flagged: %v", p)
	}
	for name, s := range map[string]*Schema{
		"negative index without a spec": {Format: "text", TraceIDField: "-1"},
		"unknown value":                 {Format: "text", Extract: map[string]ExtractSpec{"colour": {}}},
		"non-ASCII chars":               {Format: "text", LevelField: "3", Extract: map[string]ExtractSpec{"level": {Chars: "«»"}}},
		"bad fallback":                  {Format: "text", Extract: map[string]ExtractSpec{"trace_id": {Fallback: "("}}},
		"fallback without a group":      {Format: "text", Extract: map[string]ExtractSpec{"trace_id": {Fallback: `id=\w+`}}},
	} {
		if len(s.Validate()) == 0 {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestExtractSpecYAMLRoundTrip(t *testing.T) {
	s := &Schema{Format: "text", LevelField: "-2", Extract: map[string]ExtractSpec{"level": {Suffix: "]", CutAt: "/", Fallback: `lvl=(\w+)`}}}
	y, err := s.ToYAML()
	if err != nil {
		t.Fatal(err)
	}
	back, err := LoadSchemaFromBytes([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Extract, s.Extract) || back.LevelField != "-2" {
		t.Errorf("round trip lost data:\n%s\n%+v", y, back)
	}
}
