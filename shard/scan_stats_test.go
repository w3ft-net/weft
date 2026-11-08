package shard

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/proto/weftpb"
)

// A trailer after the end sentinel carries what the query scanned. Readers
// that stop at the sentinel, which is every reader deployed before this one,
// never see it.
func TestScanStats_FollowTheSentinelWithoutDisturbingOlderReaders(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteResultRow(&buf, &weftpb.ResultRow{Raw: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteStreamEnd(&buf); err != nil {
		t.Fatal(err)
	}
	if err := WriteScanStats(&buf, ScanStats{RowsScanned: 1_234_567, BytesRead: 89}); err != nil {
		t.Fatal(err)
	}

	r := bytes.NewReader(buf.Bytes())
	rows, err := ReadAllResultRows(r)
	if err != nil || len(rows) != 1 {
		t.Fatalf("older reader: rows=%d err=%v", len(rows), err)
	}
	got, ok, err := ReadScanStats(r)
	if err != nil || !ok {
		t.Fatalf("ReadScanStats: ok=%v err=%v", ok, err)
	}
	if got != (ScanStats{RowsScanned: 1_234_567, BytesRead: 89}) {
		t.Errorf("stats = %+v", got)
	}
}

// A Lambda from before the trailer ends at the sentinel: no stats, no error.
func TestScanStats_AbsentFromAnOlderLambda(t *testing.T) {
	var buf bytes.Buffer
	WriteStreamEnd(&buf)
	r := bytes.NewReader(buf.Bytes())
	if _, err := ReadAllResultRows(r); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadScanStats(r); ok || err != nil {
		t.Errorf("ok=%v err=%v, want neither", ok, err)
	}
}

// The completion line is what turns a day of traffic into calibration data.
func TestQueryToSink_LogsWhatTheLambdaScanned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-protobuf-stream")
		WriteResultRow(w, &weftpb.ResultRow{Raw: "a"})
		WriteStreamEnd(w)
		WriteScanStats(w, ScanStats{RowsScanned: 42_000, BytesRead: 7_000})
	}))
	defer srv.Close()

	cs, err := NewColdShard("obs-scan", []string{"vimgym"}, "s3://b/x.parquet", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	sink := &testSink{}
	out := captureLog(t, func() {
		cs.QueryToSink(context.Background(), "SELECT raw, ts FROM logs WHERE customer_id = 1", nil, sink)
	})
	if len(sink.rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(sink.rows))
	}
	for _, want := range []string{"obs-scan", "rows_scanned=42000", "bytes_read=7000"} {
		if !strings.Contains(out, want) {
			t.Errorf("completion log lacks %q: %s", want, out)
		}
	}
}

func TestQueryToSink_CompletesWithoutATrailer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "")
		WriteStreamEnd(w)
	}))
	defer srv.Close()

	cs, err := NewColdShard("obs-old", []string{"vimgym"}, "s3://b/x.parquet", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	sink := &testSink{}
	out := captureLog(t, func() {
		cs.QueryToSink(context.Background(), "SELECT raw, ts FROM logs WHERE customer_id = 1", nil, sink)
	})
	if strings.Contains(out, "failed") || strings.Contains(out, "error") {
		t.Errorf("an older Lambda's stream logged a failure: %s", out)
	}
	if !strings.Contains(out, "query completed") {
		t.Errorf("no completion line: %s", out)
	}
}

// reportingSink is a testSink that also wants the scan stats.
type reportingSink struct {
	testSink
	shardID string
	stats   ScanStats
	calls   int
}

func (r *reportingSink) ReportScan(shardID string, s ScanStats) {
	r.shardID, r.stats = shardID, s
	r.calls++
}

// The stats reach a sink that asks for them, once; a sink that doesn't is unaffected.
func TestQueryToSink_ReportsScanToASinkThatWantsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteResultRow(w, &weftpb.ResultRow{Raw: "a"})
		WriteStreamEnd(w)
		WriteScanStats(w, ScanStats{RowsScanned: 42_000, BytesRead: 7_000})
	}))
	defer srv.Close()
	cs, err := NewColdShard("rep-1", []string{"vimgym"}, "s3://b/x.parquet", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	sink := &reportingSink{}
	cs.QueryToSink(context.Background(), "SELECT raw, ts FROM logs WHERE customer_id = 1", nil, sink)
	if sink.calls != 1 || sink.shardID != "rep-1" || sink.stats.RowsScanned != 42_000 {
		t.Errorf("reported %d times: shard=%q stats=%+v", sink.calls, sink.shardID, sink.stats)
	}
}

// A Lambda from before the trailer reports nothing, which meters as zero.
func TestQueryToSink_ReportsNothingWithoutATrailer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteStreamEnd(w)
	}))
	defer srv.Close()
	cs, err := NewColdShard("rep-2", []string{"vimgym"}, "s3://b/x.parquet", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	sink := &reportingSink{}
	cs.QueryToSink(context.Background(), "SELECT raw, ts FROM logs WHERE customer_id = 1", nil, sink)
	if sink.calls != 0 {
		t.Errorf("reported %d times without a trailer", sink.calls)
	}
}
