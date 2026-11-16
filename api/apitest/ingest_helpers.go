package apitest

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/w3ft-net/weft/api"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// fakeIngestCoord stands in for the coordinator's IngestStream RPC —
// the same path weftd, JSONAdapter, and HerokuAdapter all forward
// into. Records every row received and the "authorization" metadata
// each stream carried, so tests can assert SyslogServer forwarded the
// connection's token unchanged.
type fakeIngestCoord struct {
	pb.UnimplementedWeftCoordinatorServer

	mu        sync.Mutex
	rows      []*pb.LogRow
	authSeen  []string
	traceSeen []string
	calls     int
}

func (f *fakeIngestCoord) IngestStream(stream grpc.ClientStreamingServer[pb.LogRow, pb.IngestStreamResponse]) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	f.mu.Lock()
	f.calls++
	if vals := md.Get("authorization"); len(vals) > 0 {
		f.authSeen = append(f.authSeen, vals[0])
	}
	if vals := md.Get("weft-trace-id"); len(vals) > 0 {
		f.traceSeen = append(f.traceSeen, vals[0])
	}
	f.mu.Unlock()
	for {
		row, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&pb.IngestStreamResponse{})
		}
		if err != nil {
			return err
		}
		f.mu.Lock()
		f.rows = append(f.rows, row)
		f.mu.Unlock()
	}
}

func (f *fakeIngestCoord) snapshot() (rows []*pb.LogRow, authSeen []string, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pb.LogRow(nil), f.rows...), append([]string(nil), f.authSeen...), f.calls
}

// startFakeCoord dials a fresh gRPC server implementing only
// IngestStream, returning it alongside a ready client. Mirrors the
// fakeCoord pattern in shell_test.go.
func startFakeCoord(t *testing.T) (*fakeIngestCoord, pb.WeftCoordinatorClient) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fake := &fakeIngestCoord{}
	grpcSrv := grpc.NewServer()
	pb.RegisterWeftCoordinatorServer(grpcSrv, fake)
	go grpcSrv.Serve(lis)
	t.Cleanup(grpcSrv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(weftsdk.GRPC.UnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(weftsdk.GRPC.StreamClientInterceptor()))
	if err != nil {
		t.Fatalf("dial fake coord: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return fake, pb.NewWeftCoordinatorClient(conn)
}

// waitForRows polls fake until n rows arrive or the timeout expires.
func waitForRows(t *testing.T, fake *fakeIngestCoord, n int) []*pb.LogRow {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, _, _ := fake.snapshot()
		if len(rows) >= n || time.Now().After(deadline) {
			return rows
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// herokuFrame builds an octet-counted RFC5424 frame the way Heroku's drain
// protocol delivers them: `<bytes> <message>`.
func herokuFrame(msg string) string {
	return fmt.Sprintf("%d %s", len(msg), msg)
}

// hbody concatenates frames for a multi-frame body.
func hbody(frames ...string) string {
	return strings.Join(frames, "")
}

// firehoseBody builds a Firehose HTTP-endpoint-delivery request body
// from a set of already-encoded (base64, maybe-gzipped) CloudWatch
// envelope records.
func firehoseBody(requestID string, records ...string) string {
	b, _ := json.Marshal(map[string]any{
		"requestId": requestID,
		"timestamp": 1750000000000,
		"records": func() []map[string]string {
			out := make([]map[string]string, len(records))
			for i, r := range records {
				out[i] = map[string]string{"data": r}
			}
			return out
		}(),
	})
	return string(b)
}

// gzipEnvelope base64-encodes gzip-compressed envelope JSON — the
// "still gzipped" branch of the open gzip-or-not question.
func gzipEnvelope(t *testing.T, envelope map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// plainEnvelope base64-encodes uncompressed envelope JSON — the "plain
// JSON" branch.
func plainEnvelope(t *testing.T, envelope map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func dataMessageEnvelope(logGroup string, messages ...string) map[string]any {
	events := make([]map[string]any, len(messages))
	for i, m := range messages {
		events[i] = map[string]any{"id": "evt", "timestamp": 1750000000000, "message": m}
	}
	return map[string]any{
		"messageType": "DATA_MESSAGE",
		"owner":       "123456789012",
		"logGroup":    logGroup,
		"logStream":   "stream1",
		"logEvents":   events,
	}
}

// herokuLines builds n single-frame-per-line Heroku body frames for service.
func herokuLines(service string, n int) string {
	frames := make([]string, n)
	for i := range frames {
		msg := fmt.Sprintf(`<134>1 2026-04-30T12:00:00.000000+00:00 host %s web.1 - - INFO req-%d hello`, service, i)
		frames[i] = herokuFrame(msg)
	}
	return hbody(frames...)
}

func postHeroku(t *testing.T, srv *api.Server, token, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/ingest/heroku/"+token, strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.IngestHandler(api.HerokuAdapter{}).ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
}
