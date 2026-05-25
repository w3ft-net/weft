package shard

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureLog redirects the standard logger for the duration of fn and
// returns everything written. The default logger writes to os.Stderr;
// queryToSinkRemote uses log.Printf, so this is how we assert a
// previously-silent failure now leaves a breadcrumb.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()
	fn()
	return buf.String()
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
		cs.QueryToSink("SELECT raw, ts FROM logs WHERE customer = 'kevlar'", nil, sink)
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
		cs.QueryToSink("SELECT raw, ts FROM logs WHERE customer = 'kevlar'", nil, sink)
	})

	if !sink.sentinelReceived {
		t.Error("expected sentinel even on connection failure")
	}
	if !strings.Contains(out, "obs-conn") || !strings.Contains(out, "lambda call failed") {
		t.Fatalf("connection failure was not logged; log = %q", out)
	}
}

// TestQueryToSink_SuccessIsSilent guards against log spam: a normal
// successful query (clean io.EOF stream end) must not log anything.
func TestQueryToSink_SuccessIsSilent(t *testing.T) {
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
		cs.QueryToSink("SELECT raw, ts FROM logs WHERE namespace = 'vimgym' AND customer = 'kevlar'", nil, sink)
	})

	if len(sink.rows) == 0 {
		t.Fatal("expected rows")
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("successful query should not log; got %q", out)
	}
}
