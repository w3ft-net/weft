package service

import (
	"context"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/coordinator"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	storepkg "github.com/graybeardcorp/weft/store"
)

// An agent that reports its version when it polls a source's status is
// recorded against that source, per host. An agent from before the field
// existed reports nothing and leaves no record: "unknown", which the API
// treats as not safe for a schema only newer agents can read.
func TestGetSourceStatusRecordsReportingAgents(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()
	srv := &Server{Coord: coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}}), Store: store}

	cust, err := store.FindOrCreateOrg("agent-versions")
	if err != nil {
		t.Fatal(err)
	}
	store.SeedDevSubscription(cust.ID)
	ns, _ := store.FindOrCreateNamespace(cust.ID, "ops")
	tok, _, err := store.CreateAgentToken(cust.ID, ns.ID, "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPendingLogGroup(ns.ID, "api", "h", "d", `["x"]`, "{}", 1, false); err != nil {
		t.Fatal(err)
	}
	lg, err := store.FindLogGroup(cust.ID, "ops", "api")
	if err != nil || lg == nil {
		t.Fatal(lg, err)
	}

	poll := func(version, instance string) {
		t.Helper()
		if _, err := srv.GetSourceStatus(context.Background(), &pb.GetSourceStatusRequest{
			Token: tok, Service: "api", AgentVersion: version, Instance: instance,
		}); err != nil {
			t.Fatal(err)
		}
	}

	poll("", "")         // an agent that predates the fields
	poll("", "old-host") // ...even if it somehow named its host
	if agents, _ := store.RecentAgents(lg.ID, time.Hour); len(agents) != 0 {
		t.Fatalf("an agent that reported no version was recorded: %+v", agents)
	}

	poll("0.28.0", "web-1")
	poll("0.23.0", "web-2")
	agents, err := store.RecentAgents(lg.ID, time.Hour)
	if err != nil || len(agents) != 2 {
		t.Fatalf("agents = %+v, %v; want both hosts", agents, err)
	}
	if v, n := storepkg.OldestAgentVersion(agents); v != "0.23.0" || n != 2 {
		t.Errorf("oldest = %q of %d, want 0.23.0 of 2", v, n)
	}
}
