// Package sources is the producer side of a log source: what a kind of source
// (weftd, Heroku, Render, a Firehose delivery stream, a bare IngestStream
// client) does differently from the others, and the registry that names them.
//
// A Producer holds only the rules that differ per kind: the state a new source
// starts in, whether live traffic fills its sample, what a resample request does
// to it, and a check on a schema before it is approved. Transports (an HTTPS
// route, a TLS syslog listener) are interfaces each transport's own package
// declares and a producer type also satisfies; the registry finds those by
// type. Weftd is a native client of the coordinator's gRPC stream, so it has no
// transport interface.
//
// The registered name is the kind stored on a source row. The package imports
// only the standard library, so a build that links it stays cgo-free.
package sources

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/w3ft-net/weft/release"
)

// Status is where a source is in getting a schema.
type Status string

const (
	// Sampling: an agent has announced the source but is still accumulating
	// toward a full sample. Not yet configurable.
	Sampling Status = "sampling"
	// Pending: a full sample waits for a person to approve a schema.
	Pending Status = "pending"
	// Configured: the source is live. It may still have no schema (raw lines
	// are queryable as is), and a resample may be filling a new sample.
	Configured Status = "configured"
)

// Source is the part of a source row a producer's rules read.
type Source struct {
	ID     uint
	Status Status
	// ResampleRequested is set while a fresh sample is being collected for a
	// configured source.
	ResampleRequested bool
}

// Registration is what a caller knows when it creates a source.
type Registration struct {
	// Locator and Params are the producer's address for the source (a Firehose
	// delivery stream's log group, a region) and its other parameters.
	Locator string
	Params  map[string]string
	// Partial is set when the first sample the source arrived with is not yet
	// complete.
	Partial bool
}

// Start is where a new source begins.
type Start struct {
	Status            Status
	ResampleRequested bool
}

// ResamplePlan is what a resample request does to a source, beyond setting the
// resample flag and the sample size, which every kind gets.
type ResamplePlan struct {
	// Status is the status the request moves the source to; empty leaves it.
	Status Status
	// ResetSample clears the sample, its count and its detection, and stamps
	// the sample as changed now.
	ResetSample bool
}

// SchemaNeeds is what a schema about to be approved asks of whatever reads the
// source with it.
type SchemaNeeds struct {
	// MinClientVersion is the oldest agent version that can interpret the
	// schema; empty means any.
	MinClientVersion string
}

// SchemaError refuses a schema. Message is for the person approving it; Err,
// when set, is the cause to log.
type SchemaError struct {
	Message string
	Err     error
}

func (e *SchemaError) Error() string { return e.Message }
func (e *SchemaError) Unwrap() error { return e.Err }

// Producer is what a kind of source does differently from the others.
type Producer interface {
	// InitialState is where a new source of this kind starts.
	InitialState(Registration) Start
	// CollectsSample reports whether a batch of live rows should add to the
	// source's sample. The cap on lines is the caller's, the same for every
	// kind.
	CollectsSample(Source) bool
	// Resample is what a resample request does to the source. The request is
	// refused earlier, for every kind alike, when the source is already
	// collecting.
	Resample(Source) ResamplePlan
	// CheckSchema vets a schema about to be approved for the source. A refusal
	// is a *SchemaError.
	CheckSchema(ctx context.Context, s Source, need SchemaNeeds) error
}

// Push is the defaults of a source whose sample is filled from the rows pushed
// to it: Heroku, Render, OTLP, Firehose and bare IngestStream clients. A
// producer embeds it and adds its transport methods.
//
// Such a source is born configured, because ingest never waits on a schema (raw
// lines are queryable as is), with a resample requested, which is how it gets a
// sample: the ingest path accumulates live lines into it while it keeps
// ingesting, and a full sample surfaces it for schema review.
type Push struct{}

func (Push) InitialState(Registration) Start {
	return Start{Status: Configured, ResampleRequested: true}
}

func (Push) CollectsSample(s Source) bool { return s.Status == Configured && s.ResampleRequested }

func (Push) Resample(Source) ResamplePlan { return ResamplePlan{ResetSample: true} }

func (Push) CheckSchema(context.Context, Source, SchemaNeeds) error { return nil }

// AgentFleet is what Agent needs to know about the agents reading a source.
type AgentFleet interface {
	// Oldest is the lowest version among the agents that polled the source
	// within window, and how many there are; ("", 0) for none.
	Oldest(ctx context.Context, sourceID uint, window time.Duration) (version string, n int, err error)
	// Fleets summarizes the agents of each of the sources, keyed by source id;
	// a source no agent polled within window has no entry.
	Fleets(ctx context.Context, sourceIDs []uint, window time.Duration) (map[uint]Fleet, error)
}

// Fleet summarizes the agents reading one source: how many, and the lowest and
// highest version among them.
type Fleet struct {
	Count  int
	Oldest string
	Newest string
}

// KindWeftd is the registered name of the agent-sampled kind of source.
const KindWeftd = "weftd"

// DefaultAgentWindow is how recently an agent must have polled to count as
// reading a source.
const DefaultAgentWindow = 7 * 24 * time.Hour

// Agent is the defaults of a source an agent samples for: weftd. The agent
// submits the sample, so live rows never fill it, and its sources are the only
// ones that are ever pending or sampling.
type Agent struct {
	// Fleet answers which agents read a source. Without it CheckSchema approves
	// nothing that needs a client version.
	Fleet AgentFleet
	// Window defaults to DefaultAgentWindow.
	Window time.Duration
}

func (a Agent) InitialState(r Registration) Start {
	if r.Partial {
		return Start{Status: Sampling}
	}
	return Start{Status: Pending}
}

func (Agent) CollectsSample(Source) bool { return false }

// Resample sets the flag a waiting agent polls for. A pending source goes back
// to sampling with its sample cleared; a configured one keeps ingesting on its
// schema, and the agent's fresh sample replaces the old.
func (Agent) Resample(s Source) ResamplePlan {
	if s.Status == Pending {
		return ResamplePlan{Status: Sampling, ResetSample: true}
	}
	return ResamplePlan{}
}

// CheckSchema refuses a schema that agents too old to interpret would ignore
// silently. An agent that never reported a version counts as unknown, not as
// capable.
func (a Agent) CheckSchema(ctx context.Context, s Source, need SchemaNeeds) error {
	if need.MinClientVersion == "" {
		return nil
	}
	window := a.Window
	if window == 0 {
		window = DefaultAgentWindow
	}
	if a.Fleet == nil {
		return &SchemaError{Message: "could not check which agent versions read this source; try again"}
	}
	oldest, n, err := a.Fleet.Oldest(ctx, s.ID, window)
	if err != nil {
		return &SchemaError{Message: "could not check which agent versions read this source; try again", Err: err}
	}
	switch {
	case n == 0:
		return &SchemaError{Message: fmt.Sprintf("no agent has reported for this source in the last %d days, so its weftd version is unknown; "+
			"this schema needs weftd %s or newer. Upgrade weftd on the hosts shipping this source (they report their version "+
			"as they run), then configure again.", int(window.Hours()/24), need.MinClientVersion)}
	case release.VersionNewer(need.MinClientVersion, oldest):
		return &SchemaError{Message: fmt.Sprintf("the oldest agent reading this source is weftd %s (%d host(s) reporting); this schema needs weftd %s or newer. "+
			"Upgrade weftd on those hosts, then configure again.", oldest, n, need.MinClientVersion)}
	}
	return nil
}

// Registry names the producers of a build. It is a value, passed to whatever
// serves them, not global state.
type Registry struct {
	byName map[string]Producer
}

func NewRegistry() *Registry { return &Registry{byName: map[string]Producer{}} }

// Register adds p under name, which is the kind stored on its sources.
func (r *Registry) Register(name string, p Producer) error {
	switch {
	case name == "":
		return errors.New("sources: a producer needs a name")
	case p == nil:
		return fmt.Errorf("sources: producer %q is nil", name)
	}
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("sources: producer %q is already registered", name)
	}
	r.byName[name] = p
	return nil
}

// Get is the producer registered under name.
func (r *Registry) Get(name string) (Producer, bool) {
	p, ok := r.byName[name]
	return p, ok
}

// Names are the registered names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Each calls fn, in name order, with every producer that also satisfies T. A
// server uses it to find the producers of its transport: the syslog server asks
// for the ones that implement its stream interface.
func Each[T any](r *Registry, fn func(name string, p T)) {
	for _, name := range r.Names() {
		if t, ok := r.byName[name].(T); ok {
			fn(name, t)
		}
	}
}

// Inert is the rules for a kind nothing registers (a retired kind still on old
// rows): it collects no sample, a resample request only sets the flag, and any
// schema is approved.
type Inert struct{}

func (Inert) InitialState(Registration) Start { return Start{Status: Pending} }

func (Inert) CollectsSample(Source) bool { return false }

func (Inert) Resample(s Source) ResamplePlan {
	if s.Status == Pending {
		return ResamplePlan{Status: Sampling, ResetSample: true}
	}
	return ResamplePlan{}
}

func (Inert) CheckSchema(context.Context, Source, SchemaNeeds) error { return nil }

// Core is the registry of the kinds that exist today: weftd, and the push
// kinds, direct and kinesis, which differ only in name (a legacy kind keeps its
// stored value). fleet answers weftd's schema check.
func Core(fleet AgentFleet) *Registry {
	r := NewRegistry()
	_ = r.Register(KindWeftd, Agent{Fleet: fleet})
	_ = r.Register("direct", Push{})
	_ = r.Register("kinesis", Push{})
	return r
}

// Producer is the one registered under name, or Inert when there is none.
func (r *Registry) Producer(name string) Producer {
	if r != nil {
		if p, ok := r.byName[name]; ok {
			return p
		}
	}
	return Inert{}
}
