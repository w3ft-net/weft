package ingest

import (
	"reflect"
	"testing"
)

func segs(t *testing.T, p *Preview, line int) [][2]string {
	t.Helper()
	var out [][2]string
	for _, s := range p.Lines[line] {
		out = append(out, [2]string{string(s.Field), s.Text})
	}
	return out
}

var previewLines = []string{
	"2026-10-16T18:18:55.649Z INFO request id=abc123 dur=8ms",
	"2026-10-16T18:18:56.100Z WARN request id=def456 dur=442ms",
	"2026-10-16T18:18:57.000Z INFO started",
}

func previewSchema() *Schema {
	return &Schema{
		Format:          "text",
		TimestampField:  "1",
		TimestampFormat: "2006-01-02T15:04:05Z07:00",
		TimestampFields: 1,
		LevelField:      "2",
		TraceIDField:    "none",
		TraceIDPattern:  `id=(\w+)`,
	}
}

func TestSchemaPreviewMarksEveryExtractedFieldWhereItSits(t *testing.T) {
	p, err := SchemaPreview(previewSchema(), previewLines)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{
		{"timestamp", "2026-10-16T18:18:55.649Z"},
		{"", " "},
		{"level", "INFO"},
		{"", " request "},
		{"trace_id", "abc123"},
		{"", " dur=8ms"},
	}
	// the trace value is found by its pattern, so the "id=" before it stays plain
	want[3] = [2]string{"", " request id="}
	if got := segs(t, p, 0); !reflect.DeepEqual(got, want) {
		t.Errorf("line 0:\n got %q\nwant %q", got, want)
	}
	if want := []ProbeField{ProbeTimestamp, ProbeLevel, ProbeTraceID}; !reflect.DeepEqual(p.Fields, want) {
		t.Errorf("fields = %v, want %v", p.Fields, want)
	}
}

func TestSchemaPreviewLeavesALineWithoutTheValuePlainThere(t *testing.T) {
	p, err := SchemaPreview(previewSchema(), previewLines)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range p.Lines[2] {
		if s.Field == ProbeTraceID {
			t.Errorf("line 2 has no trace id, got a trace segment %q", s.Text)
		}
	}
	// every line's segments put back together are the line itself
	for i, l := range previewLines {
		var joined string
		for _, s := range p.Lines[i] {
			joined += s.Text
		}
		if joined != l {
			t.Errorf("line %d: segments join to %q, want %q", i, joined, l)
		}
	}
}

func TestSchemaPreviewKeepsTheEarlierFieldWhereTwoOverlap(t *testing.T) {
	s := previewSchema()
	// a duration that claims the whole timestamp field too
	s.DurationNsField = "1"
	s.DurationUnit = "text"
	p, err := SchemaPreview(s, previewLines[:1])
	if err != nil {
		t.Fatal(err)
	}
	for _, seg := range p.Lines[0] {
		if seg.Field == ProbeDurationNs {
			t.Errorf("the timestamp already holds those characters: %+v", seg)
		}
	}
}

func TestSchemaPreviewWithNothingExtractedIsPlain(t *testing.T) {
	p, err := SchemaPreview(&Schema{Format: "text"}, previewLines)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Fields) != 0 || len(p.Lines[0]) != 1 || p.Lines[0][0].Text != previewLines[0] {
		t.Errorf("got %+v", p)
	}
}
