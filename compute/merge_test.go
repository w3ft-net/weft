package compute

import (
	"reflect"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

func row(ts int64, raw string) *pb.ResultRow {
	return &pb.ResultRow{TsUnixNs: ts, Raw: raw}
}

func TestMergeRows_TsAsc(t *testing.T) {
	perShard := map[string][]*pb.ResultRow{
		"a": {row(1, "one"), row(3, "three"), row(5, "five")},
		"b": {row(2, "two"), row(4, "four"), row(6, "six")},
	}
	got := mergeRows(perShard, "ts")
	want := []string{"one", "two", "three", "four", "five", "six"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMergeRows_TsDesc(t *testing.T) {
	perShard := map[string][]*pb.ResultRow{
		"a": {row(5, "five"), row(3, "three"), row(1, "one")},
		"b": {row(6, "six"), row(4, "four"), row(2, "two")},
	}
	got := mergeRows(perShard, "ts DESC")
	want := []string{"six", "five", "four", "three", "two", "one"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMergeRows_Raw(t *testing.T) {
	perShard := map[string][]*pb.ResultRow{
		"a": {row(0, "alpha"), row(0, "cherry")},
		"b": {row(0, "bravo"), row(0, "delta")},
	}
	got := mergeRows(perShard, "raw")
	want := []string{"alpha", "bravo", "cherry", "delta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMergeRows_EmptyShard(t *testing.T) {
	perShard := map[string][]*pb.ResultRow{
		"a": {row(1, "one"), row(3, "three")},
		"b": {},
	}
	got := mergeRows(perShard, "ts")
	want := []string{"one", "three"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMergeRows_UnknownKey_FallsBackToConcat(t *testing.T) {
	perShard := map[string][]*pb.ResultRow{
		"a": {row(1, "one")},
	}
	got := mergeRows(perShard, "not_a_column")
	want := []string{"one"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseMergeKey(t *testing.T) {
	cases := []struct {
		in       string
		wantCol  string
		wantDesc bool
	}{
		{"ts", "ts", false},
		{"ts DESC", "ts", true},
		{"raw", "raw", false},
		{"raw DESC", "raw", true},
		{"raw desc", "raw", true},
		{"", "", false},
	}
	for _, tc := range cases {
		col, desc := parseMergeKey(tc.in)
		if col != tc.wantCol || desc != tc.wantDesc {
			t.Errorf("parseMergeKey(%q) = (%q, %v), want (%q, %v)", tc.in, col, desc, tc.wantCol, tc.wantDesc)
		}
	}
}
