package shard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/internal/logtest"
	weftsdk "github.com/w3ft-net/weft-go"
)

// captureLog redirects the standard logger for the duration of fn and
// returns everything written. The default logger writes to os.Stderr;
// queryToSinkRemote uses log.Printf, so this is how we assert a
// previously-silent failure now leaves a breadcrumb.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	return logtest.Capture(fn)
}

// TestQueryToSink_LogsStreamReadError: a 200 response whose body is not
// a valid protobuf stream (the pre-fix lambda's smuggled-error shape)
// must be logged, not silently swallowed as zero rows.
func TestQueryToSink_LogsStreamReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-protobuf-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("\nerror: query: No files found that match the pattern\n"))
	}))
	defer srv.Close()

	cs, err := NewColdShard("obs-stream", []string{"vimgym"}, "s3://b/x.parquet", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	sink := &testSink{}
	out := captureLog(t, func() {
		cs.QueryToSink(context.Background(), "SELECT raw, ts FROM logs WHERE customer_id = 1", nil, sink)
	})

	if !sink.sentinelReceived {
		t.Error("expected sentinel even on failure")
	}
	if len(sink.rows) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(sink.rows))
	}
	if !strings.Contains(out, "obs-stream") || !strings.Contains(out, "stream read error") {
		t.Fatalf("stream read failure was not logged; log = %q", out)
	}
}

// TestQueryToSink_LogsConnectionFailure: an unreachable lambda must be
// logged. Before the fix, s.client.Do errors returned silently.
func TestQueryToSink_LogsConnectionFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // now connections are refused immediately

	cs, err := NewColdShard("obs-conn", []string{"vimgym"}, "s3://b/x.parquet", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	sink := &testSink{}
	out := captureLog(t, func() {
		cs.QueryToSink(context.Background(), "SELECT raw, ts FROM logs WHERE customer_id = 1", nil, sink)
	})

	if !sink.sentinelReceived {
		t.Error("expected sentinel even on connection failure")
	}
	if !strings.Contains(out, "obs-conn") || !strings.Contains(out, "lambda call failed") {
		t.Fatalf("connection failure was not logged; log = %q", out)
	}
}

// TestQueryToSink_LogsCompletionAlways supersedes the old "success is
// silent" contract: unlike a live shard (which stays silent unless
// actively traced — see shard/service/server.go), a cold-shard query
// always logs its completion (shard id, duration, row count), since
// archived-data queries are the rarer, costlier path most likely to
// explain hidden latency — the whole motivation for this feature. An
// untraced query's line carries no trace_id field.
func TestQueryToSink_LogsCompletionAlways(t *testing.T) {
	parquetPath := exportToParquet(t, testRows())
	srv := httptest.NewServer(lambdaHandler(t))
	defer srv.Close()

	cs, err := NewColdShard("obs-ok", []string{"vimgym"}, parquetPath, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	sink := &testSink{}
	out := captureLog(t, func() {
		cs.QueryToSink(context.Background(), "SELECT raw, ts FROM logs WHERE namespace = 'vimgym' AND customer_id = 1", nil, sink)
	})

	if len(sink.rows) == 0 {
		t.Fatal("expected rows")
	}
	if !strings.Contains(out, "obs-ok") || !strings.Contains(out, "query completed") {
		t.Fatalf("successful query should log completion; got %q", out)
	}
	if strings.Contains(out, "trace_id") {
		t.Fatalf("untraced query should not log a trace_id field; got %q", out)
	}
}

// TestQueryToSink_TraceIDInLogWhenPresent: a query made under an
// ambient trace ID gets that ID appended to the same completion line.
func TestQueryToSink_TraceIDInLogWhenPresent(t *testing.T) {
	parquetPath := exportToParquet(t, testRows())
	srv := httptest.NewServer(lambdaHandler(t))
	defer srv.Close()

	cs, err := NewColdShard("obs-traced", []string{"vimgym"}, parquetPath, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	ctx := weftsdk.ContextWithTraceID(context.Background(), "test-trace-123")
	sink := &testSink{}
	out := captureLog(t, func() {
		cs.QueryToSink(ctx, "SELECT raw, ts FROM logs WHERE namespace = 'vimgym' AND customer_id = 1", nil, sink)
	})

	if len(sink.rows) == 0 {
		t.Fatal("expected rows")
	}
	if !strings.Contains(out, "obs-traced") || !strings.Contains(out, "query completed") {
		t.Fatalf("successful query should log completion; got %q", out)
	}
	if !strings.Contains(out, `"trace_id":"test-trace-123"`) {
		t.Fatalf("traced query's log should include trace_id; got %q", out)
	}
}
