package compute

import (
	"testing"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

func fieldRow(raw string, count int32, fields ...string) *pb.ResultRow {
	return &pb.ResultRow{Raw: raw, Fields: fields, FieldCount: &count}
}

// RebuildRecord makes the record a stage that reads only some fields sees:
// the fields it names where it expects them, a placeholder in every position
// before the last one named, and the real count and last field when it reads
// those. It has to equal what the shard's text-record SQL built.
func TestRebuildRecord(t *testing.T) {
	for _, c := range []struct {
		name string
		spec *pb.FieldSpec
		row  *pb.ResultRow
		want string
	}{
		{"positions only", &pb.FieldSpec{Positions: []int32{1, 3}}, fieldRow("", 5, "a", "c"), "a _ c"},
		{"a position the line lacks", &pb.FieldSpec{Positions: []int32{1, 3}}, fieldRow("", 2, "a", ""), "a _"},
		{"a line with no fields", &pb.FieldSpec{Positions: []int32{1, 3}}, fieldRow("", 0, "", ""), ""},
		{"nothing read", &pb.FieldSpec{}, fieldRow("", 4), ""},
		{"NF pads to the real count", &pb.FieldSpec{Positions: []int32{1}, Nf: true}, fieldRow("", 4, "a"), "a _ _ _"},
		{"NF alone", &pb.FieldSpec{Nf: true}, fieldRow("", 3), "_ _ _"},
		{"last field after the named ones", &pb.FieldSpec{Positions: []int32{1}, Last: true}, fieldRow("", 4, "a", "d"), "a _ _ d"},
		{"last field is a named one", &pb.FieldSpec{Positions: []int32{2}, Last: true}, fieldRow("", 2, "b", "b"), "_ b"},
		{"last field inside the named range", &pb.FieldSpec{Positions: []int32{1, 4}, Last: true}, fieldRow("", 3, "a", "", "c"), "a _ c"},
		{"last field with one field", &pb.FieldSpec{Last: true}, fieldRow("", 1, "x"), "x"},
		{"last field of a line with no fields is the line", &pb.FieldSpec{Positions: []int32{1}, Last: true}, fieldRow("  \t ", 0, "", ""), "  \t "},
	} {
		if got := RebuildRecord(c.spec, c.row); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// A row in field form becomes the line the stage reads as it enters the job;
// a row in raw form, from a shard without the field form, is left alone.
func TestJobSendRow_RebuildsFieldForm(t *testing.T) {
	j := (&Engine{}).NewJob(nil, 1, "", 0, false, false)
	j.fieldSpec = &pb.FieldSpec{Positions: []int32{1, 3}}
	field, raw := fieldRow("", 3, "a", "c"), &pb.ResultRow{Raw: "a _ c"}
	j.SendRow(field, "s1")
	j.SendRow(raw, "s2")
	if field.Raw != "a _ c" || raw.Raw != "a _ c" {
		t.Errorf("field form %q, raw form %q, want both %q", field.Raw, raw.Raw, "a _ c")
	}
}
