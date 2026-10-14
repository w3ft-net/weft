package ingest

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

func raws(rows []*pb.LogRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Raw
	}
	return out
}

func TestSplitLines(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		payload   string
		eventID   string
		want      []string
		wantSplit bool
	}{
		{"one line", "hello", "", "", []string{"hello"}, false},
		{"empty", "", "", "", []string{""}, false},
		{"trace", "Traceback:\n  File x\nValueError", "", "", []string{"Traceback:", "  File x", "ValueError"}, true},
		{"crlf", "a\r\nb\r\nc", "", "", []string{"a", "b", "c"}, true},
		{"trailing newline is not a blank line", "c\n", "", "", []string{"c"}, false},
		{"trailing crlf", "c\r\n", "", "", []string{"c"}, false},
		{"trailing cr", "c\r", "", "", []string{"c"}, false},
		{"blank line inside stays", "a\n\nb\n", "", "", []string{"a", "", "b"}, true},
		{"only newline", "\n", "", "", []string{""}, false},
		{"json payload never splits", "{\n \"a\": 1\n}", `{"a":1}`, "", []string{"{\n \"a\": 1\n}"}, false},
		{"already split passes through", "a\nb", "", "5c5f4a0e-0000-7000-8000-000000000000", []string{"a\nb"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := &pb.LogRow{Raw: tc.raw, Service: "api", Level: "ERROR", TsUnixNs: 42, TraceId: "t1", CustomerId: 7, EventId: tc.eventID}
			if tc.payload != "" {
				in.PayloadJson = []byte(tc.payload)
			}
			got := SplitLines([]*pb.LogRow{in})
			if !reflect.DeepEqual(raws(got), tc.want) {
				t.Fatalf("raws = %q, want %q", raws(got), tc.want)
			}
			if tc.wantSplit {
				id := got[0].EventId
				if _, err := uuid.Parse(id); err != nil {
					t.Fatalf("event_id %q: %v", id, err)
				}
				if uuid.MustParse(id).Version() != 7 {
					t.Fatalf("event_id %s is not a UUIDv7", id)
				}
			}
			for i, r := range got {
				if tc.wantSplit {
					if r.EventId != got[0].EventId || int(r.LineNo) != i {
						t.Fatalf("row %d: event_id=%q line_no=%d", i, r.EventId, r.LineNo)
					}
				} else if tc.eventID == "" && (r.EventId != "" || r.LineNo != 0) {
					t.Fatalf("row %d got event_id=%q line_no=%d on an unsplit event", i, r.EventId, r.LineNo)
				}
				// Every other column is the event's, copied to each line.
				if r.Service != "api" || r.Level != "ERROR" || r.TsUnixNs != 42 || r.TraceId != "t1" || r.CustomerId != 7 {
					t.Fatalf("row %d lost event columns: %+v", i, r)
				}
			}
		})
	}
}

func TestSplitLines_KeepsOrderAcrossBatch(t *testing.T) {
	in := []*pb.LogRow{{Raw: "one"}, {Raw: "a\nb"}, {Raw: "two"}, {Raw: "x\r\ny\r\nz"}}
	got := SplitLines(in)
	want := []string{"one", "a", "b", "two", "x", "y", "z"}
	if !reflect.DeepEqual(raws(got), want) {
		t.Fatalf("raws = %q, want %q", raws(got), want)
	}
	if got[1].EventId == "" || got[1].EventId == got[4].EventId {
		t.Fatalf("events must get distinct ids: %q %q", got[1].EventId, got[4].EventId)
	}
}

func TestSplitLines_NoSplitReturnsInput(t *testing.T) {
	in := []*pb.LogRow{{Raw: "a"}, {Raw: "b"}}
	got := SplitLines(in)
	if &got[0] != &in[0] {
		t.Fatal("expected the input slice back when nothing splits")
	}
}
