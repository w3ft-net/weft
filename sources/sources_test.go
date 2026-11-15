package sources

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The old per-kind rules, copied from the code they replace, so the producers
// are pinned to what that code did. Each names where it lived.

// api/adapter.go accumulateSample: who collects, by kind.
func oldCollects(kind string, status Status, resample bool) bool {
	switch kind {
	case "kinesis":
		bootstrapping := status == Pending || status == Sampling
		resampling := status == Configured && resample
		return bootstrapping || resampling
	case "direct":
		return status == Configured && resample
	}
	return false
}

// store/source.go RequestResample: the status it moves to ("" leaves it) and
// whether it resets the sample. The "already collecting" refusal is not here:
// it is the same for every kind.
func oldResample(kind string, status Status) (to Status, reset bool) {
	if status == Pending {
		to = Sampling
	}
	reset = status == Pending || kind == "kinesis" || kind == "direct"
	return
}

var allStatuses = []Status{Pending, Sampling, Configured}

func TestPush_CollectsAsDirectDid_ForEveryStateADirectSourceCanBeIn(t *testing.T) {
	// A direct source is born configured and never leaves it, so only those two
	// states are reachable; Push must give the old answer for both.
	for _, resample := range []bool{false, true} {
		got := Push{}.CollectsSample(Source{Status: Configured, ResampleRequested: resample})
		if want := oldCollects("direct", Configured, resample); got != want {
			t.Errorf("configured, resample=%v: Push collects=%v, direct did %v", resample, got, want)
		}
	}
}

func TestPush_CollectsAsKinesisDid_ForConfiguredSources(t *testing.T) {
	for _, resample := range []bool{false, true} {
		got := Push{}.CollectsSample(Source{Status: Configured, ResampleRequested: resample})
		if want := oldCollects("kinesis", Configured, resample); got != want {
			t.Errorf("configured, resample=%v: Push collects=%v, kinesis did %v", resample, got, want)
		}
	}
}

// The one intended change: kinesis used to collect while pending or sampling.
// A Push source is born configured, so none is ever in those states, and the
// hosted migration flips existing ones.
func TestPush_NeverCollectsForAPendingOrSamplingSource(t *testing.T) {
	for _, st := range []Status{Pending, Sampling} {
		for _, resample := range []bool{false, true} {
			if (Push{}).CollectsSample(Source{Status: st, ResampleRequested: resample}) {
				t.Errorf("%s resample=%v: Push collects; a Push source is never in this state", st, resample)
			}
		}
		if !oldCollects("kinesis", st, false) {
			t.Fatalf("the old kinesis rule no longer collects while %s; this test's premise is stale", st)
		}
	}
}

func TestPush_ResamplesAsDirectAndKinesisDid_WhenConfigured(t *testing.T) {
	want := ResamplePlan{ResetSample: true}
	if got := (Push{}).Resample(Source{Status: Configured}); got != want {
		t.Errorf("Push resample = %+v, want %+v", got, want)
	}
	for _, kind := range []string{"direct", "kinesis"} {
		to, reset := oldResample(kind, Configured)
		if (ResamplePlan{Status: to, ResetSample: reset}) != want {
			t.Errorf("%s resample used to be (%q, %v), not the Push plan", kind, to, reset)
		}
	}
}

func TestPush_StartsConfiguredWithAResampleRequested(t *testing.T) {
	// store.EnsureServiceLogGroup's row: configured, resample requested now.
	got := Push{}.InitialState(Registration{Locator: "svc"})
	if got != (Start{Status: Configured, ResampleRequested: true}) {
		t.Errorf("start = %+v", got)
	}
	// An explicit registration (a Firehose log group) starts the same way.
	if got2 := (Push{}).InitialState(Registration{Locator: "/aws/lambda/x", Params: map[string]string{"region": "us-east-2"}}); got2 != got {
		t.Errorf("explicit registration start = %+v, want %+v", got2, got)
	}
}

func TestPush_ApprovesAnySchema(t *testing.T) {
	if err := (Push{}).CheckSchema(context.Background(), Source{}, SchemaNeeds{MinClientVersion: "9.9.9"}); err != nil {
		t.Errorf("CheckSchema: %v", err)
	}
}

func TestAgent_StartsPendingOrSamplingByWhetherTheFirstSampleIsComplete(t *testing.T) {
	// store.UpsertPendingLogGroup's create branch: sampling when partial.
	if got := (Agent{}).InitialState(Registration{}); got != (Start{Status: Pending}) {
		t.Errorf("complete sample: %+v", got)
	}
	if got := (Agent{}).InitialState(Registration{Partial: true}); got != (Start{Status: Sampling}) {
		t.Errorf("partial sample: %+v", got)
	}
}

func TestAgent_NeverCollectsFromLiveRows(t *testing.T) {
	for _, st := range allStatuses {
		for _, resample := range []bool{false, true} {
			if oldCollects("weftd", st, resample) || (Agent{}).CollectsSample(Source{Status: st, ResampleRequested: resample}) {
				t.Errorf("%s resample=%v: a weftd source collects from rows", st, resample)
			}
		}
	}
}

func TestAgent_ResamplesAsWeftdDid(t *testing.T) {
	for _, st := range []Status{Pending, Configured} { // sampling is refused earlier, for every kind
		to, reset := oldResample("weftd", st)
		got := Agent{}.Resample(Source{Status: st})
		if got != (ResamplePlan{Status: to, ResetSample: reset}) {
			t.Errorf("%s: Agent resample = %+v, weftd did (%q, reset=%v)", st, got, to, reset)
		}
	}
}

type fleet struct {
	version string
	n       int
	err     error
	window  time.Duration
}

func (f *fleet) Oldest(_ context.Context, _ uint, w time.Duration) (string, int, error) {
	f.window = w
	return f.version, f.n, f.err
}

func (f *fleet) Fleets(context.Context, []uint, time.Duration) (map[uint]Fleet, error) {
	return nil, f.err
}

func TestAgent_CheckSchema(t *testing.T) {
	ctx := context.Background()
	need := SchemaNeeds{MinClientVersion: "0.20.0"}
	cases := []struct {
		name    string
		fleet   *fleet
		need    SchemaNeeds
		wantErr string // substring; "" approves
	}{
		{"no client version needed", &fleet{}, SchemaNeeds{}, ""},
		{"every agent is new enough", &fleet{version: "0.20.0", n: 3}, need, ""},
		{"a newer agent", &fleet{version: "0.31.0", n: 1}, need, ""},
		{"the oldest agent is too old", &fleet{version: "0.18.0", n: 2}, need, "oldest agent reading this source is weftd 0.18.0 (2 host(s) reporting)"},
		{"no agent has reported", &fleet{}, need, "no agent has reported for this source in the last 7 days"},
	}
	for _, c := range cases {
		err := Agent{Fleet: c.fleet}.CheckSchema(ctx, Source{ID: 1}, c.need)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.wantErr)
		}
		var se *SchemaError
		if err != nil && !errors.As(err, &se) {
			t.Errorf("%s: %T is not a *SchemaError", c.name, err)
		}
	}
}

func TestAgent_CheckSchema_AFailedLookupRefusesAndKeepsTheCause(t *testing.T) {
	cause := errors.New("db down")
	f := &fleet{err: cause}
	err := Agent{Fleet: f}.CheckSchema(context.Background(), Source{}, SchemaNeeds{MinClientVersion: "0.20.0"})
	var se *SchemaError
	if !errors.As(err, &se) || !errors.Is(err, cause) || !strings.Contains(err.Error(), "try again") {
		t.Fatalf("err = %v", err)
	}
}

func TestAgent_CheckSchema_UsesTheConfiguredWindow(t *testing.T) {
	f := &fleet{version: "1.0.0", n: 1}
	_ = Agent{Fleet: f, Window: 48 * time.Hour}.CheckSchema(context.Background(), Source{}, SchemaNeeds{MinClientVersion: "0.1.0"})
	if f.window != 48*time.Hour {
		t.Errorf("window = %v", f.window)
	}
	_ = Agent{Fleet: f}.CheckSchema(context.Background(), Source{}, SchemaNeeds{MinClientVersion: "0.1.0"})
	if f.window != DefaultAgentWindow {
		t.Errorf("default window = %v, want %v", f.window, DefaultAgentWindow)
	}
}

// A producer type embeds a default and adds what is its own.
type heroku struct {
	Push
	limit int64
}

func (h heroku) Route() string { return "POST /api/ingest/heroku/{token}" }

type router interface{ Route() string }

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("heroku", heroku{limit: 1}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("weftd", Agent{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("heroku", Push{}); err == nil {
		t.Error("a duplicate name was accepted")
	}
	if err := r.Register("", Push{}); err == nil {
		t.Error("an empty name was accepted")
	}
	if err := r.Register("nil", nil); err == nil {
		t.Error("a nil producer was accepted")
	}

	if p, ok := r.Get("heroku"); !ok || p.InitialState(Registration{}).Status != Configured {
		t.Errorf("Get heroku: %v %v", p, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("Get found an unregistered name")
	}
	if got := strings.Join(r.Names(), ","); got != "heroku,weftd" {
		t.Errorf("names = %s", got)
	}

	// A server finds the producers of its transport by the interface it declares.
	var routes []string
	Each(r, func(name string, p router) { routes = append(routes, name+" "+p.Route()) })
	if len(routes) != 1 || routes[0] != "heroku POST /api/ingest/heroku/{token}" {
		t.Errorf("routers = %v, want only heroku (weftd has no transport interface)", routes)
	}
}

// The package is linked into processes that build with CGO_ENABLED=0.
func TestPackageLoadsWithoutCgo(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go list -deps with CGO_ENABLED=0: %v\n%s", err, out)
	}
}

func TestCore_RegistersTheExistingKinds(t *testing.T) {
	r := Core(&fleet{})
	if got := strings.Join(r.Names(), ","); got != "direct,kinesis,weftd" {
		t.Errorf("names = %s", got)
	}
	if _, ok := r.Producer("weftd").(Agent); !ok {
		t.Errorf("weftd is %T", r.Producer("weftd"))
	}
	if _, ok := r.Producer("kinesis").(Push); !ok {
		t.Errorf("kinesis is %T", r.Producer("kinesis"))
	}
}

func TestInert_IsTheRulesOfAKindNothingRegisters(t *testing.T) {
	p := Core(nil).Producer("cloudwatch") // retired in migration 077
	if p.CollectsSample(Source{Status: Configured, ResampleRequested: true}) {
		t.Error("an unregistered kind collects a sample")
	}
	if got := p.Resample(Source{Status: Configured}); got != (ResamplePlan{}) {
		t.Errorf("configured resample = %+v", got)
	}
	if got := p.Resample(Source{Status: Pending}); got != (ResamplePlan{Status: Sampling, ResetSample: true}) {
		t.Errorf("pending resample = %+v", got)
	}
	if err := p.CheckSchema(context.Background(), Source{}, SchemaNeeds{MinClientVersion: "9"}); err != nil {
		t.Errorf("CheckSchema: %v", err)
	}
}
