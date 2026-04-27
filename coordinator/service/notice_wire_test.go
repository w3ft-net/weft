package service

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/kevlarcon/weft/auth"
	"github.com/kevlarcon/weft/coordinator"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// captureStream is a minimal grpc.ServerStreamingServer stub that
// records every Send(). Only the methods the Query handler calls
// have meaningful implementations.
type captureStream struct {
	grpc.ServerStream
	ctx       context.Context
	responses []*pb.CoordinatorQueryResponse
}

func newCaptureStream(ctx context.Context) *captureStream {
	return &captureStream{ctx: ctx}
}

func (s *captureStream) Context() context.Context              { return s.ctx }
func (s *captureStream) Send(r *pb.CoordinatorQueryResponse) error {
	s.responses = append(s.responses, r)
	return nil
}
func (s *captureStream) SetHeader(metadata.MD) error  { return nil }
func (s *captureStream) SendHeader(metadata.MD) error { return nil }
func (s *captureStream) SetTrailer(metadata.MD)      {}
func (s *captureStream) SendMsg(m any) error          { return nil }
func (s *captureStream) RecvMsg(m any) error          { return nil }

// TestQueryRPC_EmitsNoticesOnFirstTmpWrite is the wire-shape
// integration: when a /tmp write triggers a notice, the gRPC
// response carries that notice in its `notices` field. If anything
// in coordinator.QueryResult → service.Server.Query →
// CoordinatorQueryResponse drops the notices, this test fails.
func TestQueryRPC_EmitsNoticesOnFirstTmpWrite(t *testing.T) {
	st, cleanup := testStore(t)
	defer cleanup()

	cust, _ := st.FindOrCreateCustomer("notice-wire")
	st.SeedDevSubscription(cust.Name)
	user, err := st.CreateUser("alice-wire@example.test", cust.ID)
	if err != nil {
		t.Fatal(err)
	}

	sh, err := shard.NewDuckDBShard("shard-notice-wire", "")
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()
	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{
		Shards: []shard.Shard{sh},
		Store:  st,
	})
	srv := &Server{Coord: coord, Store: st}

	// Inject the authenticated caller into the context (the auth
	// interceptor would do this in production).
	ctx := auth.WithClaims(context.Background(), &auth.Claims{
		CustomerName: cust.Name,
		UserID:       user.ID,
	})

	req := &pb.CoordinatorQueryRequest{
		Pipeline:    `echo first > /tmp/wire`,
		UserId:      user.ID,
		TimeStartNs: time.Now().Add(-time.Hour).UnixNano(),
		TimeEndNs:   time.Now().Add(time.Hour).UnixNano(),
	}

	stream := newCaptureStream(ctx)
	if err := srv.Query(req, stream); err != nil {
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
