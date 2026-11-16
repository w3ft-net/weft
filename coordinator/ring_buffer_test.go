package coordinator

import (
	"testing"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// Drained rows must not stay reachable from the backing array. With the
// prod capacity of 1M slots, a slot that keeps its pointer after Drain
// pins that row until the write head laps it — retained heap grows with
// ingest volume up to capacity × row size, which OOMs the coordinator.
func TestRingBufferDrainReleasesRows(t *testing.T) {
	r := NewRingBuffer(8)
	r.Write([]Row{{LogRow: &pb.LogRow{Raw: "a"}}, {LogRow: &pb.LogRow{Raw: "b"}}, {LogRow: &pb.LogRow{Raw: "c"}}})

	if got := r.Drain(2); len(got) != 2 {
		t.Fatalf("Drain(2) returned %d rows", len(got))
	}
	if got := r.Drain(10); len(got) != 1 {
		t.Fatalf("Drain(10) returned %d rows", len(got))
	}

	for i, row := range r.buf {
		if row.LogRow != nil {
			t.Errorf("slot %d still retains drained row %q", i, row.Raw)
		}
	}
}

// A wrapped buffer must release exactly the drained slots and keep the
// unread ones intact.
func TestRingBufferDrainReleasesRowsAcrossWrap(t *testing.T) {
	r := NewRingBuffer(4)
	r.Write([]Row{{LogRow: &pb.LogRow{Raw: "1"}}, {LogRow: &pb.LogRow{Raw: "2"}}, {LogRow: &pb.LogRow{Raw: "3"}}})
	r.Drain(3)
	r.Write([]Row{{LogRow: &pb.LogRow{Raw: "4"}}, {LogRow: &pb.LogRow{Raw: "5"}}, {LogRow: &pb.LogRow{Raw: "6"}}}) // wraps: slots 3,0,1

	got := r.Drain(2)
	if len(got) != 2 || got[0].Raw != "4" || got[1].Raw != "5" {
		t.Fatalf("Drain(2) = %v, want rows 4,5", got)
	}

	var retained []string
	for _, row := range r.buf {
		if row.LogRow != nil {
			retained = append(retained, row.Raw)
		}
	}
	if len(retained) != 1 || retained[0] != "6" {
		t.Errorf("buffer retains %v, want only the unread row [6]", retained)
	}
}
