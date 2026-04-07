package shard

import (
	"bytes"
	"io"
	"testing"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

func TestProtoStream_RoundTrip(t *testing.T) {
	rows := []*pb.LogRow{
		{LineNum: 1, Raw: "test row 1", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: time.Now().UnixNano()},
		{LineNum: 2, Raw: "test row 2", App: "callbox", Level: "ERROR", Customer: "kevlar", TsUnixNs: time.Now().UnixNano()},
		{LineNum: 3, Raw: "test row 3", App: "vimgym", Level: "WARN", Customer: "kevlar", TsUnixNs: time.Now().UnixNano(), PayloadJson: []byte(`{"status":500}`)},
	}

	var buf bytes.Buffer

	// Write all rows + sentinel.
	for _, row := range rows {
		if err := WriteLogRow(&buf, row); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := WriteStreamEnd(&buf); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	// Read them back.
	result, err := ReadAllLogRows(&buf)
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
		if r.App != rows[i].App {
			t.Errorf("row %d: expected App=%q, got %q", i, rows[i].App, r.App)
		}
		if r.TsUnixNs != rows[i].TsUnixNs {
			t.Errorf("row %d: timestamp mismatch", i)
		}
	}
}

func TestProtoStream_EmptyStream(t *testing.T) {
	var buf bytes.Buffer
	WriteStreamEnd(&buf)

	rows, err := ReadAllLogRows(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(rows))
	}
}

func TestProtoStream_SingleRow(t *testing.T) {
	var buf bytes.Buffer
	row := &pb.LogRow{LineNum: 42, Raw: "single", App: "test"}
	WriteLogRow(&buf, row)
	WriteStreamEnd(&buf)

	result, err := ReadAllLogRows(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].LineNum != 42 {
		t.Fatalf("expected 1 row with LineNum=42, got %v", result)
	}
}

func TestProtoStream_ReadLogRow_EOF(t *testing.T) {
	// Empty reader returns io.EOF.
	_, err := ReadLogRow(&bytes.Buffer{})
	if err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

func BenchmarkProtoStream_Write1K(b *testing.B) {
	row := &pb.LogRow{LineNum: 1, Raw: "benchmark row with realistic content", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: time.Now().UnixNano()}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		for j := 0; j < 1000; j++ {
			WriteLogRow(&buf, row)
		}
		WriteStreamEnd(&buf)
	}
}

func BenchmarkProtoStream_Read1K(b *testing.B) {
	row := &pb.LogRow{LineNum: 1, Raw: "benchmark row with realistic content", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: time.Now().UnixNano()}
	var source bytes.Buffer
	for j := 0; j < 1000; j++ {
		WriteLogRow(&source, row)
	}
	WriteStreamEnd(&source)
	data := source.Bytes()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ReadAllLogRows(bytes.NewReader(data))
	}
}
