package mem

import (
	"context"
	"testing"

	"github.com/w3ft-net/weft/backends/dlq"
	"github.com/w3ft-net/weft/backends/dlq/dlqstoretest"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape/shapetest"
)

func TestDLQConformance(t *testing.T) {
	dlqstoretest.Run(t, func(t *testing.T) dlq.Store { return NewDLQ() })
}

// A batch that cannot be decoded stays, is reported, and the others replay.
func TestDLQReplay_ABatchItCannotDecodeStaysAndTheOthersReplay(t *testing.T) {
	s := NewDLQ()
	ctx := context.Background()
	s.batches = append(s.batches, dlqBatch{id: -1, payload: []byte{0x00, 0xff}})
	if _, err := s.Write(ctx, shapetest.Added(1), []*pb.LogRow{{Namespace: "weft", Service: "coordinator", Raw: "good"}}); err != nil {
		t.Fatal(err)
	}
	var got []dlq.Entry
	n, err := s.Replay(ctx, func(e []dlq.Entry) { got = append(got, e...) })
	if err == nil || n != 1 || len(got) != 1 || got[0].Raw != "good" {
		t.Fatalf("Replay = %d, %v, sink saw %v; want the good row and an error for the other", n, err, got)
	}
	if len(s.batches) != 1 || s.batches[0].id != -1 {
		t.Errorf("batches remain %v, want only the undecodable one", s.batches)
	}
}
