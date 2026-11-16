package service

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/w3ft-net/weft/coordinator"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// captureStream is a minimal grpc.ServerStreamingServer stub that
// records every Send(). Only the methods the Query handler calls
// have meaningful implementations.
type captureStream struct {
	grpc.ServerStream
	ctx       context.Context
	responses []*pb.CoordinatorExecuteResponse
}

func newCaptureStream(ctx context.Context) *captureStream {
	return &captureStream{ctx: ctx}
}

func (s *captureStream) Context() context.Context { return s.ctx }
func (s *captureStream) Send(r *pb.CoordinatorExecuteResponse) error {
	s.responses = append(s.responses, r)
	return nil
}
func (s *captureStream) SetHeader(metadata.MD) error  { return nil }
func (s *captureStream) SendHeader(metadata.MD) error { return nil }
func (s *captureStream) SetTrailer(metadata.MD)       {}
func (s *captureStream) SendMsg(m any) error          { return nil }
func (s *captureStream) RecvMsg(m any) error          { return nil }

// TestQueryRPC_EmitsNoticesOnFirstTmpWrite is the wire-shape
// integration: when a /tmp write triggers a notice, the gRPC
// response carries that notice in its `notices` field. If anything
// in coordinator.QueryResult → service.Server.Query →
// CoordinatorExecuteResponse drops the notices, this test fails.
func TestQueryRPC_EmitsNoticesOnFirstTmpWrite(t *testing.T) {
	env := newSvcEnv(t)
	srv := env.server(coordinator.CoordinatorConfig{}, nil)

	// The caller the auth interceptor would put in the context in production.
	ctx := env.callerCtx(context.Background(), "alice-wire")

	req := &pb.CoordinatorExecuteRequest{
		Pipeline:    `echo first > /tmp/wire`,
		TimeStartNs: time.Now().Add(-time.Hour).UnixNano(),
		TimeEndNs:   time.Now().Add(time.Hour).UnixNano(),
	}

	stream := newCaptureStream(ctx)
	if err := srv.Execute(req, stream); err != nil {
		t.Fatalf("Query: %v", err)
	}

	// Expect at least one response with a non-empty Notices field
	// containing the tmp_48h_expiry notice.
	var found *pb.Notice
	for _, resp := range stream.responses {
		for _, n := range resp.Notices {
			if n.Key == "tmp_48h_expiry" {
				found = n
			}
		}
	}
	if found == nil {
		t.Fatalf("no tmp_48h_expiry notice in any response; got %d responses", len(stream.responses))
	}
	if found.Kind != "warning" {
		t.Errorf("notice kind: got %q, want warning", found.Kind)
	}
	if found.Title == "" || found.Body == "" {
		t.Errorf("notice content: got Title=%q Body=%q, want non-empty", found.Title, found.Body)
	}
}
