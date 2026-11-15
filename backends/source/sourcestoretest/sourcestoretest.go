// Package sourcestoretest is the conformance suite for source.Store: the
// behaviour every implementation of the namespaces-and-sources domain
// (Postgres for the hosted build, SQLite for the open one) must show. An
// implementation's test calls Run with a constructor.
package sourcestoretest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sources"
)

// Subject is one store under test, fresh and empty.
type Subject struct {
	Store source.Store
	// Owner owns the namespaces the suite creates. The open build's is the
	// empty value.
	Owner shape.Added
	// Other is a second owner, for a store that has owners to tell apart. Nil
	// for one that has none.
	Other *shape.Added
}

// Run runs the suite, building a fresh Subject for each test.
func Run(t *testing.T, newSubject func(t *testing.T) Subject) {
	cases := []struct {
		name string
		fn   func(*testing.T, Subject)
	}{
		{"Namespaces", namespaces},
		{"NamespacesBelongToTheirOwner", namespaceOwners},
		{"DeleteNamespaceRemovesItsSources", deleteNamespace},
		{"AddSource", addSource},
		{"RegisterSourceStartsAsItsKindDoes", registerSource},
		{"EnsureSource", ensureSource},
		{"DeleteSource", deleteSource},
		{"ConfigureSource", configureSource},
		{"SourceSchema", sourceSchema},
		{"FindServiceByLocator", findServiceByLocator},
		{"ListSources", listSources},
		{"SubmitSample", submitSample},
		{"RequestResample", requestResample},
		{"UpdateDetection", updateDetection},
		{"ServicesInBucket", servicesInBucket},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newSubject(t)) })
	}
}

func mustNS(t *testing.T, s Subject, name string) *source.Namespace {
	t.Helper()
	ns, err := s.Store.FindOrCreateNamespace(s.Owner, name)
	if err != nil {
		t.Fatalf("FindOrCreateNamespace %s: %v", name, err)
	}
	return ns
}

func find(t *testing.T, s Subject, ns, svc string) *source.Source {
	t.Helper()
	got, err := s.Store.FindSource(s.Owner, ns, svc)
	if err != nil {
		t.Fatalf("FindSource %s/%s: %v", ns, svc, err)
	}
	if got == nil {
		t.Fatalf("FindSource %s/%s: not found", ns, svc)
	}
	return got
}

func namespaces(t *testing.T, s Subject) {
	st := s.Store
	if _, ok, err := st.GetNamespace(s.Owner, "ops"); ok || err != nil {
		t.Fatalf("GetNamespace of an absent one: ok=%v err=%v", ok, err)
	}
	a := mustNS(t, s, "ops")
	if a.ID == 0 || a.Name != "ops" {
		t.Fatalf("namespace: %+v", a)
	}
	again := mustNS(t, s, "ops")
	if again.ID != a.ID {
		t.Fatalf("FindOrCreateNamespace twice: ids %d and %d", a.ID, again.ID)
	}
	mustNS(t, s, "app")

	got, ok, err := st.GetNamespace(s.Owner, "ops")
	if err != nil || !ok || got.ID != a.ID {
		t.Fatalf("GetNamespace: %+v ok=%v err=%v", got, ok, err)
	}
	name, ok, err := st.NamespaceName(s.Owner, a.ID)
	if err != nil || !ok || name != "ops" {
		t.Fatalf("NamespaceName: %q ok=%v err=%v", name, ok, err)
	}
	if _, ok, _ := st.NamespaceName(s.Owner, a.ID+1000); ok {
		t.Fatal("NamespaceName found an id that does not exist")
	}
	list, err := st.ListNamespaces(s.Owner)
	if err != nil || len(list) != 2 || list[0].Name != "app" || list[1].Name != "ops" {
		t.Fatalf("ListNamespaces: %+v err=%v, want app then ops", list, err)
	}
}

func namespaceOwners(t *testing.T, s Subject) {
	if s.Other == nil {
		t.Skip("the store has no owners to tell apart")
	}
	a := mustNS(t, s, "ops")
	b, err := s.Store.FindOrCreateNamespace(*s.Other, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("two owners got the same namespace")
	}
	if _, ok, _ := s.Store.GetNamespace(*s.Other, "nope"); ok {
		t.Fatal("found an absent namespace")
	}
	if list, _ := s.Store.ListNamespaces(*s.Other); len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("the other owner lists %+v", list)
	}
	if _, ok, _ := s.Store.NamespaceName(*s.Other, a.ID); ok {
		t.Fatal("an owner resolved another's namespace id")
	}
}

func deleteNamespace(t *testing.T, s Subject) {
	st := s.Store
	ns := mustNS(t, s, "ops")
	if err := st.AddSource(context.Background(), ns.ID, "direct", "api", source.SourceSpec{Locator: "api"}, "app: api\n"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteNamespace(context.Background(), s.Owner, "ops"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.GetNamespace(s.Owner, "ops"); ok {
		t.Fatal("the namespace is still there")
	}
	if got, err := st.FindSource(s.Owner, "ops", "api"); err != nil || got != nil {
		t.Fatalf("its source survived: %+v err=%v", got, err)
	}
	// The name can be created again.
	again := mustNS(t, s, "ops")
	if again.ID == ns.ID {
		t.Error("a deleted namespace's id was reused")
	}
	if err := st.DeleteNamespace(context.Background(), s.Owner, "absent"); !errors.Is(err, source.ErrNotFound) {
		t.Fatalf("deleting an absent namespace: %v, want ErrNotFound", err)
	}
}

func addSource(t *testing.T, s Subject) {
	ctx := context.Background()
	ns := mustNS(t, s, "ops")
	spec := source.SourceSpec{Locator: "/aws/lambda/x", Params: map[string]string{"region": "us-east-2"}}
	if err := s.Store.AddSource(ctx, ns.ID, "kinesis", "x", spec, "app: x\nformat: text\n"); err != nil {
		t.Fatal(err)
	}
	got := find(t, s, "ops", "x")
	if got.Kind != "kinesis" || got.Status != sources.Configured || got.Locator != spec.Locator ||
		got.Params["region"] != "us-east-2" || got.SchemaYAML != "app: x\nformat: text\n" || !got.Enabled ||
		got.Namespace != "ops" || got.NamespaceID != ns.ID || got.Service != "x" {
		t.Fatalf("source: %+v", got)
	}

	// Adding again reconfigures it: the kind, the address and the schema change,
	// and the sample goes.
	if err := s.Store.RequestResample(ns.ID, "x", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SubmitSample(ns.ID, "x", source.SampleSubmission{SampleJSON: `["a"]`, DetectionJSON: `{}`, Collected: 1, Partial: true}); err != nil {
		t.Fatal(err)
	}
	if got := find(t, s, "ops", "x"); got.Sample == "" {
		t.Fatalf("setup: the sample did not land: %+v", got)
	}
	if err := s.Store.AddSource(ctx, ns.ID, "weftd", "x", source.SourceSpec{Locator: "y"}, "app: x\nformat: json\n"); err != nil {
		t.Fatal(err)
	}
	got = find(t, s, "ops", "x")
	if got.Kind != "weftd" || got.Locator != "y" || got.SchemaYAML != "app: x\nformat: json\n" || got.Status != sources.Configured || got.Sample != "" || got.Detection != "" {
		t.Fatalf("reconfigured source: %+v", got)
	}
	if list, _ := s.Store.ListSources(s.Owner, source.Filter{}); len(list) != 1 {
		t.Fatalf("adding twice made %d sources", len(list))
	}
}

func registerSource(t *testing.T, s Subject) {
	ns := mustNS(t, s, "ops")
	spec := source.SourceSpec{Locator: "/aws/lambda/y", Params: map[string]string{"region": "eu-west-1"}}
	if err := s.Store.RegisterSource(ns.ID, "kinesis", "y", spec); err != nil {
		t.Fatal(err)
	}
	got := find(t, s, "ops", "y")
	// A push source starts configured and asking for a sample (package sources).
	if got.Status != sources.Configured || got.ResampleRequestedAt == nil || got.SchemaYAML != "" ||
		got.Locator != spec.Locator || got.Params["region"] != "eu-west-1" || got.Kind != "kinesis" || !got.Enabled {
		t.Fatalf("registered source: %+v", got)
	}
	// Idempotent, and it does not re-map an existing source.
	if err := s.Store.RegisterSource(ns.ID, "kinesis", "y", source.SourceSpec{Locator: "other"}); err != nil {
		t.Fatalf("registering twice: %v", err)
	}
	if got := find(t, s, "ops", "y"); got.Locator != spec.Locator {
		t.Fatalf("a second registration re-mapped the source to %q", got.Locator)
	}
	// A kind whose producer starts sources pending.
	if err := s.Store.RegisterSource(ns.ID, "weftd", "w", source.SourceSpec{Locator: "w"}); err != nil {
		t.Fatal(err)
	}
	if got := find(t, s, "ops", "w"); got.Status != sources.Pending || got.ResampleRequestedAt != nil {
		t.Fatalf("a weftd source starts pending: %+v", got)
	}
}

func ensureSource(t *testing.T, s Subject) {
	ns := mustNS(t, s, "ops")
	if err := s.Store.EnsureSource(ns.ID, "api"); err != nil {
		t.Fatal(err)
	}
	got := find(t, s, "ops", "api")
	if got.Kind != "direct" || got.Status != sources.Configured || got.ResampleRequestedAt == nil || got.Locator != "api" || got.SchemaYAML != "" || !got.Enabled {
		t.Fatalf("auto-onboarded source: %+v", got)
	}
	// A source of that name that already exists, of any kind, is left alone.
	if err := s.Store.AddSource(context.Background(), ns.ID, "weftd", "web", source.SourceSpec{Locator: "web"}, "app: web\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.EnsureSource(ns.ID, "web"); err != nil {
		t.Fatal(err)
	}
	if got := find(t, s, "ops", "web"); got.Kind != "weftd" || got.ResampleRequestedAt != nil {
		t.Fatalf("EnsureSource clobbered an existing source: %+v", got)
	}
}

func deleteSource(t *testing.T, s Subject) {
	ops := mustNS(t, s, "ops")
	app := mustNS(t, s, "app")
	for _, svc := range []string{"a", "b"} {
		if err := s.Store.EnsureSource(ops.ID, svc); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Store.EnsureSource(app.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.DeleteSource(ops.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Store.FindSource(s.Owner, "ops", "a"); err != nil || got != nil {
		t.Fatalf("the deleted source: %+v err=%v", got, err)
	}
	find(t, s, "ops", "b") // a sibling stays
	find(t, s, "app", "a") // so does the same name in another namespace
	if _, ok, _ := s.Store.GetNamespace(s.Owner, "ops"); !ok {
		t.Fatal("deleting a source deleted its namespace")
	}
	if err := s.Store.DeleteSource(ops.ID, "a"); err != nil {
		t.Fatalf("deleting it again: %v", err)
	}
	// The name is free: adding it again makes a fresh source.
	if err := s.Store.EnsureSource(ops.ID, "a"); err != nil {
		t.Fatal(err)
	}
	find(t, s, "ops", "a")
}

func configureSource(t *testing.T, s Subject) {
	ctx := context.Background()
	st := s.Store
	ns := mustNS(t, s, "ops")
	if err := st.ConfigureSource(ctx, ns.ID, "absent", "x"); !errors.Is(err, source.ErrNotFound) {
		t.Fatalf("configuring an absent source: %v, want ErrNotFound", err)
	}

	// A pending source with a sample becomes configured with the schema, and
	// the sample, detection, progress and resample flag go.
	if err := st.SubmitSample(ns.ID, "p", source.SampleSubmission{SampleJSON: `["a","b"]`, DetectionJSON: `{"f":1}`, Collected: 50}); err != nil {
		t.Fatal(err)
	}
	if got := find(t, s, "ops", "p"); got.Status != sources.Pending {
		t.Fatalf("setup: %+v", got)
	}
	if err := st.ConfigureSource(ctx, ns.ID, "p", "app: p\n"); err != nil {
		t.Fatal(err)
	}
	got := find(t, s, "ops", "p")
	if got.Status != sources.Configured || got.SchemaYAML != "app: p\n" || got.Sample != "" || got.Detection != "" || got.SampleCollected != 0 || got.ResampleRequestedAt != nil {
		t.Fatalf("configured source: %+v", got)
	}
	// A configured one takes a new schema.
	if err := st.ConfigureSource(ctx, ns.ID, "p", "app: p\nformat: json\n"); err != nil {
		t.Fatal(err)
	}
	if got := find(t, s, "ops", "p"); got.SchemaYAML != "app: p\nformat: json\n" {
		t.Fatalf("reconfigured: %+v", got)
	}

	// A source still sampling cannot be configured.
	if err := st.SubmitSample(ns.ID, "s", source.SampleSubmission{SampleJSON: `["a"]`, Collected: 3, Partial: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.ConfigureSource(ctx, ns.ID, "s", "x"); !errors.Is(err, source.ErrNotFound) {
		t.Fatalf("configuring a sampling source: %v, want ErrNotFound", err)
	}
}

func sourceSchema(t *testing.T, s Subject) {
	ctx := context.Background()
	ns := mustNS(t, s, "ops")
	if _, ok, err := s.Store.SourceSchema(ctx, s.Owner, "ops", "nope"); ok || err != nil {
		t.Fatalf("an absent source: ok=%v err=%v", ok, err)
	}
	if err := s.Store.RegisterSource(ns.ID, "kinesis", "k", source.SourceSpec{Locator: "k"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Store.SourceSchema(ctx, s.Owner, "ops", "k"); ok {
		t.Fatal("a source with no schema reported one")
	}
	if err := s.Store.ConfigureSource(ctx, ns.ID, "k", "app: k\n"); err != nil {
		t.Fatal(err)
	}
	yaml, ok, err := s.Store.SourceSchema(ctx, s.Owner, "ops", "k")
	if err != nil || !ok || yaml != "app: k\n" {
		t.Fatalf("SourceSchema: %q ok=%v err=%v", yaml, ok, err)
	}
	// A new schema is visible at once: nothing stale is served.
	if err := s.Store.ConfigureSource(ctx, ns.ID, "k", "app: k\nv: 2\n"); err != nil {
		t.Fatal(err)
	}
	if yaml, _, _ := s.Store.SourceSchema(ctx, s.Owner, "ops", "k"); yaml != "app: k\nv: 2\n" {
		t.Fatalf("after reconfigure the schema is %q", yaml)
	}
}

func findServiceByLocator(t *testing.T, s Subject) {
	ops := mustNS(t, s, "ops")
	other := mustNS(t, s, "other")
	if err := s.Store.RegisterSource(ops.ID, "kinesis", "cold", source.SourceSpec{Locator: "/aws/lambda/cold"}); err != nil {
		t.Fatal(err)
	}
	svc, ok, err := s.Store.FindServiceByLocator(s.Owner, "ops", "/aws/lambda/cold")
	if err != nil || !ok || svc != "cold" {
		t.Fatalf("FindServiceByLocator: %q ok=%v err=%v", svc, ok, err)
	}
	if _, ok, _ := s.Store.FindServiceByLocator(s.Owner, "ops", "/aws/lambda/none"); ok {
		t.Fatal("found a locator nobody has")
	}
	if _, ok, _ := s.Store.FindServiceByLocator(s.Owner, "other", "/aws/lambda/cold"); ok {
		t.Fatal("a locator resolved in another namespace")
	}
	_ = other
}

func listSources(t *testing.T, s Subject) {
	defer func() {
		// Every source says when it was first recorded.
		list, _ := s.Store.ListSources(s.Owner, source.Filter{})
		for _, src := range list {
			if src.CreatedAt.IsZero() || time.Since(src.CreatedAt) > time.Hour {
				t.Errorf("%s/%s CreatedAt = %v, want about now", src.Namespace, src.Service, src.CreatedAt)
			}
		}
	}()
	ctx := context.Background()
	st := s.Store
	a := mustNS(t, s, "alpha")
	b := mustNS(t, s, "beta")

	// alpha: a configured source with a schema, a push source collecting a sample
	// that has collected nothing yet, and an agent source with a full sample
	// awaiting review.
	if err := st.AddSource(ctx, a.ID, "weftd", "done", source.SourceSpec{Locator: "done"}, "app: done\n"); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureSource(a.ID, "quiet"); err != nil {
		t.Fatal(err)
	}
	if err := st.SubmitSample(a.ID, "review", source.SampleSubmission{SampleJSON: `["l"]`, Collected: 50}); err != nil {
		t.Fatal(err)
	}
	if err := st.SubmitSample(b.ID, "filling", source.SampleSubmission{SampleJSON: `["l"]`, Collected: 4, Partial: true}); err != nil {
		t.Fatal(err)
	}

	all, err := st.ListSources(s.Owner, source.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, src := range all {
		order = append(order, src.Namespace+"/"+src.Service)
	}
	if want := []string{"alpha/done", "alpha/quiet", "alpha/review", "beta/filling"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("ListSources order = %v, want %v", order, want)
	}

	pick := func(f source.Filter) []string {
		t.Helper()
		got, err := st.ListSources(s.Owner, f)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, src := range got {
			names = append(names, src.Namespace+"/"+src.Service)
		}
		return names
	}
	if got := pick(source.Filter{Namespace: "beta"}); !reflect.DeepEqual(got, []string{"beta/filling"}) {
		t.Errorf("namespace filter: %v", got)
	}
	if got := pick(source.Filter{Namespaces: []string{"alpha"}}); len(got) != 3 {
		t.Errorf("namespaces filter: %v", got)
	}
	// Awaiting a schema: the sampling and pending ones. The configured source
	// with a schema is not; the quiet push source is not either until it
	// collects a line or its sample is reset.
	if got := pick(source.Filter{AwaitingSchema: true}); !reflect.DeepEqual(got, []string{"alpha/review", "beta/filling"}) {
		t.Errorf("awaiting a schema: %v", got)
	}
	if err := st.RequestResample(a.ID, "quiet", 0); err == nil {
		t.Fatal("a source already collecting accepted a resample")
	}
	if err := st.RequestResample(a.ID, "done", 0); err != nil {
		t.Fatal(err)
	}
	if got := pick(source.Filter{AwaitingSchema: true, Namespace: "alpha"}); !reflect.DeepEqual(got, []string{"alpha/done", "alpha/review"}) {
		t.Errorf("awaiting a schema after a resample: %v", got)
	}
}

func submitSample(t *testing.T, s Subject) {
	st := s.Store
	ns := mustNS(t, s, "ops")

	// No source: created in weftd's starting state, sampling while partial.
	if err := st.SubmitSample(ns.ID, "svc", source.SampleSubmission{Instance: "i-1", Deployment: "web", SampleJSON: `["a"]`, DetectionJSON: `{"d":1}`, Collected: 7, Partial: true}); err != nil {
		t.Fatal(err)
	}
	got := find(t, s, "ops", "svc")
	if got.Kind != "weftd" || got.Status != sources.Sampling || got.Instance != "i-1" || got.Deployment != "web" ||
		got.Sample != `["a"]` || got.Detection != `{"d":1}` || got.SampleCollected != 7 || got.SampleUpdatedAt == nil || !got.Enabled {
		t.Fatalf("created by a sample: %+v", got)
	}
	// The final sample promotes it to pending.
	if err := st.SubmitSample(ns.ID, "svc", source.SampleSubmission{Instance: "i-2", SampleJSON: `["a","b"]`, Collected: 50}); err != nil {
		t.Fatal(err)
	}
	if got := find(t, s, "ops", "svc"); got.Status != sources.Pending || got.Instance != "i-2" || got.SampleCollected != 50 {
		t.Fatalf("after the final sample: %+v", got)
	}

	// Configured with no resample in flight: a stale sample is ignored.
	if err := st.ConfigureSource(context.Background(), ns.ID, "svc", "app: svc\n"); err != nil {
		t.Fatal(err)
	}
	if err := st.SubmitSample(ns.ID, "svc", source.SampleSubmission{SampleJSON: `["stale"]`, Collected: 50}); err != nil {
		t.Fatal(err)
	}
	if got := find(t, s, "ops", "svc"); got.Sample != "" || got.Status != sources.Configured {
		t.Fatalf("a stale sample landed: %+v", got)
	}

	// With a resample in flight the fresh sample lands without touching the
	// status, and the final one ends the resample.
	if err := st.RequestResample(ns.ID, "svc", 0); err != nil {
		t.Fatal(err)
	}
	if err := st.SubmitSample(ns.ID, "svc", source.SampleSubmission{SampleJSON: `["f1"]`, Collected: 10, Partial: true}); err != nil {
		t.Fatal(err)
	}
	got = find(t, s, "ops", "svc")
	if got.Status != sources.Configured || got.Sample != `["f1"]` || got.SampleCollected != 10 || got.ResampleRequestedAt == nil || got.SchemaYAML != "app: svc\n" {
		t.Fatalf("a partial resample sample: %+v", got)
	}
	if err := st.SubmitSample(ns.ID, "svc", source.SampleSubmission{SampleJSON: `["f1","f2"]`, Collected: 50}); err != nil {
		t.Fatal(err)
	}
	if got := find(t, s, "ops", "svc"); got.ResampleRequestedAt != nil || got.Sample != `["f1","f2"]` || got.Status != sources.Configured {
		t.Fatalf("the final resample sample: %+v", got)
	}
}

func requestResample(t *testing.T, s Subject) {
	ctx := context.Background()
	st := s.Store
	ns := mustNS(t, s, "ops")

	if err := st.RequestResample(ns.ID, "absent", 0); !errors.Is(err, source.ErrNotFound) {
		t.Fatalf("an absent source: %v, want ErrNotFound", err)
	}

	// A push source: the sample, count and detection are reset, the flag is set,
	// the status is left, and the size is what was asked for.
	if err := st.AddSource(ctx, ns.ID, "direct", "push", source.SourceSpec{Locator: "push"}, "app: push\n"); err != nil {
		t.Fatal(err)
	}
	if err := st.RequestResample(ns.ID, "push", 100); err != nil {
		t.Fatal(err)
	}
	got := find(t, s, "ops", "push")
	if got.Status != sources.Configured || got.ResampleRequestedAt == nil || got.Target() != 100 || got.Sample != "" || got.SampleCollected != 0 || got.SampleUpdatedAt == nil {
		t.Fatalf("a push source after a resample request: %+v", got)
	}
	if err := st.RequestResample(ns.ID, "push", 0); !errors.Is(err, source.ErrAlreadySampling) {
		t.Fatalf("a resample already in flight: %v, want ErrAlreadySampling", err)
	}

	// Sizes outside the range are refused before anything else.
	for _, n := range []int{source.SampleTarget - 1, source.MaxSampleTarget + 1} {
		if err := st.RequestResample(ns.ID, "push", n); !errors.Is(err, source.ErrBadSampleSize) {
			t.Errorf("size %d: %v, want ErrBadSampleSize", n, err)
		}
	}

	// An agent source: a pending one goes to sampling with its sample cleared; a
	// configured one keeps its sample and only gets the flag.
	if err := st.SubmitSample(ns.ID, "agent", source.SampleSubmission{SampleJSON: `["a"]`, DetectionJSON: `{}`, Collected: 50}); err != nil {
		t.Fatal(err)
	}
	if err := st.RequestResample(ns.ID, "agent", 0); err != nil {
		t.Fatal(err)
	}
	got = find(t, s, "ops", "agent")
	if got.Status != sources.Sampling || got.Sample != "" || got.SampleCollected != 0 || got.ResampleRequestedAt == nil || got.Target() != source.SampleTarget {
		t.Fatalf("a pending agent source after a resample request: %+v", got)
	}
	if err := st.RequestResample(ns.ID, "agent", 0); !errors.Is(err, source.ErrAlreadySampling) {
		t.Fatalf("a sampling source: %v, want ErrAlreadySampling", err)
	}

	if err := st.AddSource(ctx, ns.ID, "weftd", "cfg", source.SourceSpec{Locator: "cfg"}, "app: cfg\n"); err != nil {
		t.Fatal(err)
	}
	if err := st.RequestResample(ns.ID, "cfg", 0); err != nil {
		t.Fatal(err)
	}
	if got := find(t, s, "ops", "cfg"); got.Status != sources.Configured || got.ResampleRequestedAt == nil || got.SchemaYAML != "app: cfg\n" {
		t.Fatalf("a configured agent source after a resample request: %+v", got)
	}
}

func updateDetection(t *testing.T, s Subject) {
	st := s.Store
	ns := mustNS(t, s, "ops")
	if err := st.SubmitSample(ns.ID, "svc", source.SampleSubmission{SampleJSON: `["a"]`, DetectionJSON: `{"old":1}`, Collected: 50}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateDetection(ns.ID, "svc", `["a"]`, `{"new":1}`); err != nil {
		t.Fatal(err)
	}
	got := find(t, s, "ops", "svc")
	if got.Detection != `{"new":1}` || got.Sample != `["a"]` || got.Status != sources.Pending || got.SampleCollected != 50 {
		t.Fatalf("after a detection for the current sample: %+v", got)
	}
	// A late result for a sample that has since changed is dropped.
	if err := st.UpdateDetection(ns.ID, "svc", `["older"]`, `{"late":1}`); err != nil {
		t.Fatal(err)
	}
	if got := find(t, s, "ops", "svc"); got.Detection != `{"new":1}` {
		t.Fatalf("a late detection overwrote a newer one: %q", got.Detection)
	}
	// A source that is gone is a no-op, not an error.
	if err := st.UpdateDetection(ns.ID, "gone", `["a"]`, `{}`); err != nil {
		t.Fatalf("a detection for an absent source: %v", err)
	}
}

func servicesInBucket(t *testing.T, s Subject) {
	ns := mustNS(t, s, "ops")
	for _, svc := range []string{"a", "b"} {
		if err := s.Store.EnsureSource(ns.ID, svc); err != nil {
			t.Fatal(err)
		}
	}
	// No source is assigned to a bucket, so they all belong to the default one.
	got, err := s.Store.ServicesInBucket(9999, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("the default bucket's services: %+v, want both", got)
	}
	for _, g := range got {
		if g.NamespaceID != ns.ID || (g.Service != "a" && g.Service != "b") {
			t.Errorf("service %+v", g)
		}
	}
	if got, _ := s.Store.ServicesInBucket(9999, false); len(got) != 0 {
		t.Fatalf("a bucket nothing is assigned to serves %+v", got)
	}
}
