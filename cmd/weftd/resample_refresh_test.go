package main

import (
	"context"
	"sync"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// statusKeyRecorder is a fake coordinator client that records the
// Service key every GetSourceStatus call asks about and answers
// "configured" with a pending resample — the state that should make the
// refresher act. It records SubmitSample keys too, though the resample
// goroutine is a no-op here (no resampleSources entry registered).
type statusKeyRecorder struct {
	pb.WeftCoordinatorClient
	mu             sync.Mutex
	statusServices []string
}

func (r *statusKeyRecorder) GetSourceStatus(_ context.Context, req *pb.GetSourceStatusRequest, _ ...grpc.CallOption) (*pb.GetSourceStatusResponse, error) {
	r.mu.Lock()
	r.statusServices = append(r.statusServices, req.Service)
	r.mu.Unlock()
	return &pb.GetSourceStatusResponse{Status: "configured", ResampleRequested: true}, nil
}

func (r *statusKeyRecorder) lastStatusService() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.statusServices) == 0 {
		return ""
	}
	return r.statusServices[len(r.statusServices)-1]
}

// TestRefreshSourceLooksUpBareService pins the bug behind xylem's dead
// resample button: the agent's parser cache is keyed by the internal
// "namespace/service/kind" composite, but the coordinator stores log
// sources under the BARE service (GetSourceStatus → FindLogGroup matches
// on the service column alone and does not strip the kind). So the
// refresher must query by the bare service. The previous code stripped
// only the namespace, leaving "service/kind" ("celery/main"), which the
// coordinator never matches — every resample and schema refresh silently
// stranded at "unknown".
func TestRefreshSourceLooksUpBareService(t *testing.T) {
	rec := &statusKeyRecorder{}
	// fullApp is the agent's composite for a journald celery source.
	refreshSource(context.Background(), rec, "wft_test", "xylem/celery/main")

	if got := rec.lastStatusService(); got != "celery" {
		t.Fatalf("refresher asked the coordinator about %q; it keys sources on the bare service, want %q", got, "celery")
	}
}

// A hyphenated service under the usual three-segment composite must
// strip the kind and resolve to the bare service.
func TestRefreshSourceStripsKindFromHyphenatedService(t *testing.T) {
	rec := &statusKeyRecorder{}
	refreshSource(context.Background(), rec, "wft_test", "xylem/celery-beat/main")
	if got := rec.lastStatusService(); got != "celery-beat" {
		t.Fatalf("asked about %q, want %q", got, "celery-beat")
	}
}
