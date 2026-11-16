package service

import (
	"context"
	"testing"

	"github.com/w3ft-net/weft/backends/defaults"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sqlitedb"
)

// An agent's sample and status calls run over any source.Store and
// token.Store: here the open build's SQLite ones, with no Postgres.
func TestSampleAndStatus_OverTheLocalStores(t *testing.T) {
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sources, err := defaults.NewSourceBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := defaults.NewTokenBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	ns, err := sources.FindOrCreateNamespace(shape.Added{}, "ops")
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := tokens.CreateToken(shape.Added{}, ns.ID, "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Tokens: tokens, Sources: sources}
	ctx := context.Background()

	if resp, err := s.GetSourceStatus(ctx, &pb.GetSourceStatusRequest{Token: raw, Service: "api"}); err != nil || resp.Status != "unknown" {
		t.Fatalf("a source nobody announced: %+v err=%v", resp, err)
	}

	// A weftd agent announces the source by sampling it: partial pings, then the
	// complete sample.
	lines := []string{`{"a":1}`, `{"a":2}`}
	resp, err := s.SubmitSample(ctx, &pb.SubmitSampleRequest{Token: raw, Service: "api/main", Instance: "host-1", Lines: lines, Partial: true})
	if err != nil || resp.Status != "sampling" {
		t.Fatalf("a partial sample: %+v err=%v", resp, err)
	}
	st, err := s.GetSourceStatus(ctx, &pb.GetSourceStatusRequest{Token: raw, Service: "api"})
	if err != nil || st.Status != "sampling" || st.SampleCollected != 2 {
		t.Fatalf("status while sampling: %+v err=%v", st, err)
	}
	resp, err = s.SubmitSample(ctx, &pb.SubmitSampleRequest{Token: raw, Service: "api", Instance: "host-1", Lines: lines})
	if err != nil || resp.Status != "pending" {
		t.Fatalf("the complete sample: %+v err=%v", resp, err)
	}

	// An operator configures it; the agent then reads the schema.
	if err := sources.ConfigureSource(ctx, ns.ID, "api", "app: api\nformat: json\n"); err != nil {
		t.Fatal(err)
	}
	st, err = s.GetSourceStatus(ctx, &pb.GetSourceStatusRequest{Token: raw, Service: "api"})
	if err != nil || st.Status != "configured" || st.SchemaYaml != "app: api\nformat: json\n" || st.SchemaHash == "" {
		t.Fatalf("status once configured: %+v err=%v", st, err)
	}

	// A SDK-fed source needs no wizard: its final sample configures it.
	resp, err = s.SubmitSample(ctx, &pb.SubmitSampleRequest{Token: raw, Service: "sdk", Lines: lines, SdkFed: true, SdkFormat: "json"})
	if err != nil || resp.Status != "configured" {
		t.Fatalf("an SDK-fed sample: %+v err=%v", resp, err)
	}

	// A bad token is refused before any source is touched.
	if _, err := s.SubmitSample(ctx, &pb.SubmitSampleRequest{Token: "wft_00000000000000000000000000000000", Service: "api", Lines: lines}); err == nil {
		t.Error("a sample under an unknown token was accepted")
	}
}
