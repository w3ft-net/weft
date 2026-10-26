package shard

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// A result stream ends with a zero-length sentinel. One that just stops (the
// Lambda hit an error after it had sent its headers and stopped writing) is
// missing rows, and must read as an error, never as a complete answer.
func TestReadAllResultRows_StreamWithoutSentinelIsAnError(t *testing.T) {
	var rowsOnly bytes.Buffer
	WriteResultRow(&rowsOnly, &pb.ResultRow{Raw: "a", TsUnixNs: 1})

	for name, stream := range map[string]*bytes.Buffer{
		"rows then nothing": &rowsOnly,
		"nothing at all":    {},
	} {
		if rows, err := ReadAllResultRows(stream); err == nil {
			t.Errorf("%s: read %d rows with no error from a stream that never ended", name, len(rows))
		}
	}
}

func TestColdShard_RemoteQueryToSink_TruncatedStreamFailsTheShard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-protobuf-stream")
		w.WriteHeader(http.StatusOK)
		WriteResultRow(w, &pb.ResultRow{Raw: "only row", TsUnixNs: time.Now().UnixNano()})
		// no end sentinel: the handler "failed" after its headers went out
	}))
	defer srv.Close()

	cs, err := NewColdShard("truncated", nil, "", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	sink := &testSink{}
	cs.QueryToSink(context.Background(), "SELECT raw, ts FROM logs", nil, sink)
	if sink.err == nil {
		t.Fatalf("truncated stream (%d rows, no sentinel) was accepted as a complete answer", len(sink.rows))
	}
}
