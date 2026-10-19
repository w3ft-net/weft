package service

import (
	"context"
	"testing"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/coordinator"
	"github.com/graybeardcorp/weft/ingest"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// A final sample from an SDK-fed source configures the source straight away
// with a synthesized schema; no wizard step, nothing pending.
func TestSubmitSample_SDKFedConfiguresWithoutWizard(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()
	srv := &Server{Coord: coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}}), Store: store}

	cust, err := store.FindOrCreateOrg("sdk-fed")
	if err != nil {
		t.Fatal(err)
	}
	store.SeedDevSubscription(cust.ID)
	ns, _ := store.FindOrCreateNamespace(cust.ID, "ops")
	tok, _, err := store.CreateAgentToken(cust.ID, ns.ID, "agent", "")
	if err != nil {
		t.Fatal(err)
	}

	// A progress ping leaves the source sampling, not configured.
	if _, err := srv.SubmitSample(context.Background(), &pb.SubmitSampleRequest{
		Token: tok, Service: "api", Lines: []string{`{"msg":"a"}`}, Partial: true, SdkFed: true, SdkFormat: "json",
	}); err != nil {
		t.Fatal(err)
	}
	if lg, _ := store.FindLogGroup(cust.ID, "ops", "api"); lg == nil || lg.Status == "configured" {
		t.Fatalf("after a partial sample: %+v, want sampling", lg)
	}

	resp, err := srv.SubmitSample(context.Background(), &pb.SubmitSampleRequest{
		Token: tok, Service: "api", Lines: []string{`{"msg":"a"}`, `{"msg":"b"}`}, SdkFed: true, SdkFormat: "json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "configured" {
		t.Errorf("response status = %q, want configured", resp.Status)
	}
	st, err := srv.GetSourceStatus(context.Background(), &pb.GetSourceStatusRequest{Token: tok, Service: "api"})
	if err != nil || st.Status != "configured" {
		t.Fatalf("source status = %+v, %v; want configured", st, err)
	}
	sch, err := ingest.LoadSchemaFromBytes([]byte(st.SchemaYaml))
	if err != nil || !sch.SDK || sch.Format != "json" {
		t.Errorf("schema = %+v, %v; want an SDK json schema\n%s", sch, err, st.SchemaYaml)
	}
}

// A source that is already configured keeps its schema when a stale
// SDK-fed final sample arrives; only a source still awaiting one is set up.
func TestSubmitSample_SDKFedKeepsConfiguredSchema(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()
	srv := &Server{Coord: coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}}), Store: store}

	cust, _ := store.FindOrCreateOrg("sdk-fed-keep")
	store.SeedDevSubscription(cust.ID)
	ns, _ := store.FindOrCreateNamespace(cust.ID, "ops")
	tok, _, _ := store.CreateAgentToken(cust.ID, ns.ID, "agent", "")

	if err := store.UpsertPendingLogGroup(ns.ID, "api", "h", "d", `["x"]`, "{}", 1, false); err != nil {
		t.Fatal(err)
	}
	const tuned = "app: api\nenvelope: none\nformat: text\ntimestamp_field: none\n"
	if err := store.ConfigureLogGroup(context.Background(), ns.ID, "api", tuned); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.SubmitSample(context.Background(), &pb.SubmitSampleRequest{
		Token: tok, Service: "api", Lines: []string{"x"}, SdkFed: true, SdkFormat: "json",
	}); err != nil {
		t.Fatal(err)
	}
	st, _ := srv.GetSourceStatus(context.Background(), &pb.GetSourceStatusRequest{Token: tok, Service: "api"})
	if st.SchemaYaml != tuned {
		t.Errorf("schema was replaced: %q", st.SchemaYaml)
	}
}
