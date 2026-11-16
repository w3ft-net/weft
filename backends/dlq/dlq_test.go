package dlq

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// The format every store keeps: gzip, one JSON record per row, each with the
// added columns beside the row, and no group in the row itself.
func TestEncode_IsGzippedLinesOfAddedAndRow(t *testing.T) {
	rows := []*pb.LogRow{
		{Namespace: "weft", Service: "coordinator", Raw: "a"},
		{Namespace: "weft", Service: "coordinator", Raw: "b"},
	}
	b, err := Encode(shapetest.Added(1), rows)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	var recs []map[string]any
	for dec := json.NewDecoder(gz); ; {
		var rec map[string]any
		if err := dec.Decode(&rec); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
	}
	if len(recs) != len(rows) {
		t.Fatalf("%d records, want %d", len(recs), len(rows))
	}
	wantAdded, _ := json.Marshal(shapetest.Added(1))
	if got, _ := json.Marshal(recs[0]["added"]); string(got) != string(wantAdded) {
		t.Errorf("record carries the added columns %s, want %s", got, wantAdded)
	}
	row, _ := recs[0]["row"].(map[string]any)
	if row["namespace"] != "weft" || row["service"] != "coordinator" {
		t.Errorf("record fields wrong: %v", recs[0])
	}
	if _, stamped := row["customerId"]; stamped {
		t.Errorf("the row carries customer_id: %v", recs[0])
	}
}

// The envelope restores the added columns of a row that never carried tag 26.
func TestDecode_RestoresTheAddedColumnsWithoutTag26(t *testing.T) {
	b, err := Encode(shapetest.Added(7), []*pb.LogRow{{Namespace: "weft", Service: "coordinator", Raw: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(bytes.NewReader(b))
	if err != nil || len(got) != 1 {
		t.Fatalf("Decode = %v, %v", got, err)
	}
	if got[0].Added != shapetest.Added(7) || got[0].CustomerId != 0 {
		t.Errorf("replayed as %+v with customer_id %d, want group 7 and 0", got[0].Added, got[0].CustomerId)
	}
}

// A line written before the envelope is a bare LogRow, and nothing a replay can
// read says whose it is: it is refused, not replayed as the zero group's.
func TestDecodeLine_RefusesALineWrittenBeforeTheEnvelope(t *testing.T) {
	raw, err := protojson.Marshal(&pb.LogRow{Namespace: "weft", Service: "coordinator", Raw: "old", CustomerId: 12})
	if err != nil {
		t.Fatal(err)
	}
	if e, err := decodeLine(raw); err == nil {
		t.Fatalf("a line with no envelope decoded as %+v, want an error", e)
	}
}
