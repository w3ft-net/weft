package shard

import (
	"bytes"
	"io"
	"testing"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

func TestProtoStream_RoundTrip(t *testing.T) {
	rows := []*pb.ResultRow{
		{LineNum: 1, Raw: "test row 1"},
		{LineNum: 2, Raw: "test row 2"},
		{LineNum: 3, Raw: "test row 3"},
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
		if r.LineNum != rows[i].LineNum {
			t.Errorf("row %d: expected LineNum=%d, got %d", i, rows[i].LineNum, r.LineNum)
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
	row := &pb.ResultRow{LineNum: 42, Raw: "single"}
	WriteResultRow(&buf, row)
	WriteStreamEnd(&buf)

	result, err := ReadAllResultRows(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].LineNum != 42 {
		t.Fatalf("expected 1 row with LineNum=42, got %v", result)
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
	row := &pb.ResultRow{LineNum: 1, Raw: "benchmark row with realistic content"}
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
	row := &pb.ResultRow{LineNum: 1, Raw: "benchmark row with realistic content"}
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
