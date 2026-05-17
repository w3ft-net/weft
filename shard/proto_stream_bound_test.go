package shard

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// TestReadResultRow_RejectsOversizeFrame pins the compute-OOM fix.
//
// ReadResultRow read a 4-byte big-endian length straight off the wire
// and did `make([]byte, length)` with no bound. A byte-misaligned or
// truncated stream (e.g. the lambda-shard stopping mid-stream without
// the end sentinel) makes the next "length" read garbage — up to ~4 GB
// — so a single bad cold-shard response OOM-killed the whole compute
// worker (which only has 512 MB). Tiny result sets (0–340 rows) were
// irrelevant: it never got past the allocation.
//
// After the fix an absurd length must fail fast with ErrOversizeFrame
// and allocate nothing, so one corrupt shard degrades to a per-shard
// error instead of taking the process down.
func TestReadResultRow_RejectsOversizeFrame(t *testing.T) {
	var buf bytes.Buffer
	// A length one byte over the cap, followed by no data at all —
	// exactly what a truncated/misaligned stream looks like.
	binary.Write(&buf, binary.BigEndian, uint32(MaxResultRowBytes+1))

	_, err := ReadResultRow(&buf)
	if err == nil {
		t.Fatal("ReadResultRow accepted an oversize length prefix; expected ErrOversizeFrame")
	}
	if !errors.Is(err, ErrOversizeFrame) {
		t.Fatalf("ReadResultRow returned %v; want ErrOversizeFrame (no giant allocation)", err)
	}
}

// TestReadResultRow_NormalRoundTripStillWorks guards against the bound
// breaking the happy path: a real row and the end sentinel must still
// behave exactly as before.
func TestReadResultRow_NormalRoundTripStillWorks(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteResultRow(&buf, &pb.ResultRow{Raw: "hello world", TsUnixNs: 42}); err != nil {
		t.Fatalf("WriteResultRow: %v", err)
	}
	if err := WriteStreamEnd(&buf); err != nil {
		t.Fatalf("WriteStreamEnd: %v", err)
	}

	row, err := ReadResultRow(&buf)
	if err != nil {
		t.Fatalf("ReadResultRow(valid frame): %v", err)
	}
	if row.Raw != "hello world" || row.TsUnixNs != 42 {
		t.Fatalf("round-trip mismatch: got %+v", row)
	}
	if _, err := ReadResultRow(&buf); err != io.EOF {
		t.Fatalf("end sentinel: got %v, want io.EOF", err)
	}
}
