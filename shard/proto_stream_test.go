package shard

import (
	"bytes"
	"io"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

func TestProtoStream_RoundTrip(t *testing.T) {
	rows := []*pb.ResultRow{
		{Raw: "test row 1", TsUnixNs: coldT0.Add(1 * time.Second).UnixNano()},
		{Raw: "test row 2", TsUnixNs: coldT0.Add(2 * time.Second).UnixNano()},
		{Raw: "test row 3", TsUnixNs: coldT0.Add(3 * time.Second).UnixNano()},
	}

	var buf bytes.Buffer

	// Write all rows + sentinel.
	for _, row := range rows {
		if err := WriteResultRow(&buf, row); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := WriteStreamEnd(&buf); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	// Read them back.
	result, err := ReadAllResultRows(&buf)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(result))
	}

	for i, r := range result {
		if r.Raw != rows[i].Raw {
			t.Errorf("row %d: expected Raw=%q, got %q", i, rows[i].Raw, r.Raw)
		}
	}
}

func TestProtoStream_EmptyStream(t *testing.T) {
	var buf bytes.Buffer
	WriteStreamEnd(&buf)

	rows, err := ReadAllResultRows(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(rows))
	}
}

func TestProtoStream_SingleRow(t *testing.T) {
	var buf bytes.Buffer
	row := &pb.ResultRow{Raw: "single", TsUnixNs: coldT0.UnixNano()}
	WriteResultRow(&buf, row)
	WriteStreamEnd(&buf)

	result, err := ReadAllResultRows(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].Raw != "single" {
		t.Fatalf("expected 1 row with Raw=single, got %v", result)
	}
}

func TestProtoStream_ReadResultRow_EOF(t *testing.T) {
	// Empty reader returns io.EOF.
	_, err := ReadResultRow(&bytes.Buffer{})
	if err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

func BenchmarkProtoStream_Write1K(b *testing.B) {
	row := &pb.ResultRow{Raw: "benchmark row with realistic content", TsUnixNs: coldT0.UnixNano()}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		for j := 0; j < 1000; j++ {
			WriteResultRow(&buf, row)
		}
		WriteStreamEnd(&buf)
	}
}

func BenchmarkProtoStream_Read1K(b *testing.B) {
	row := &pb.ResultRow{Raw: "benchmark row with realistic content", TsUnixNs: coldT0.UnixNano()}
	var source bytes.Buffer
	for j := 0; j < 1000; j++ {
		WriteResultRow(&source, row)
	}
	WriteStreamEnd(&source)
	data := source.Bytes()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ReadAllResultRows(bytes.NewReader(data))
	}
}
