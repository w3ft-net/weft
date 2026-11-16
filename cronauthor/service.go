package cronauthor

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/cron/cronsched"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/binscript"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/planner"
	"github.com/w3ft-net/weft/shape"
)

// Service writes crons: it holds a save to the group's plan limits, refuses the
// ones that would put a text cron behind a numeric reader, lowers the pipeline so
// a fire needn't, and warns of what is worth knowing, over cron.
type Service struct {
	Crons   cron.Store
	Sources source.Store
	Bins    bin.Store
	// Entitlements says what the group is held to; nil is no limits.
	Entitlements identity.Entitlements
	// Readers lists what else reads the cron at path (<namespace>/<name>) as
	// numbers, each as the path to show, so that a cron can't be made text out
	// from under it. A deployment adds the readers it has (hosted: dashboards);
	// nil means nothing else does.
	Readers func(owner shape.Added, path string) ([]string, error)
}

// Request is one cron to save.
type Request struct {
	Owner shape.Added
	// Home is the home namespace's id, 0 for an owner-level cron.
	Home               uint
	User, Name         string
	Schedule, Pipeline string
	Enabled            bool
	// Authorize is the caller's own check of what the pipeline may read, run
	// after the plan limits and before anything is compiled or written. A cron
	// runs as the system identity, so what it may read is decided here.
	Authorize func(pipeline string) error
}

// Save writes the cron. The error may be a warning-only one (see package
// warning): the cron was saved, and the message is worth showing.
func (s Service) Save(ctx context.Context, r Request) error {
	var limits identity.Limits
	if s.Entitlements != nil {
		limits = s.Entitlements.LimitsFor(r.Owner)
	}

	// A cron that is new, or being turned back on, takes a slot; disabled crons
	// are free.
	if r.Enabled && limits.MaxCrons > 0 {
		if existing, err := s.Crons.GetWatch(r.Owner, r.Home, r.Name); err != nil || !existing.Enabled {
			count, err := s.Crons.CountEnabled(r.Owner)
			if err != nil {
				return fmt.Errorf("failed to check cron count: %w", err)
			}
			if count >= limits.MaxCrons {
				return &identity.PlanLimitError{Msg: fmt.Sprintf("cron limit reached (%d crons on %s plan)", limits.MaxCrons, limits.Plan)}
			}
		}
	}
	interval, err := cronsched.ValidateSchedule(r.Schedule)
	if err != nil {
		return err
	}
	if interval < limits.MinCronInterval {
		return &identity.PlanLimitError{Msg: fmt.Sprintf("minimum schedule interval for %s plan is %s", limits.Plan, limits.MinCronInterval)}
	}
	if r.Authorize != nil {
		if err := r.Authorize(r.Pipeline); err != nil {
			return err
		}
	}

	compiled, err := Compile(r.Pipeline)
	if err != nil {
		return err
	}
	if err := s.checkValueTypes(r.Owner, r.Home, r.Name, compiled); err != nil {
		return err
	}

	// Lower at save time so the fire path can skip preflight and planning. It
	// returns nil on any failure (an unsupported /bin pipeline, say): the save
	// still succeeds, and the fire path falls back to planning inline.
	lowered := planner.LowerForStorage(ctx, r.Pipeline, shape.Set{r.Owner.Coord()}, limits.Plan, binResolver{s.Bins, r.Owner})

	spec := cron.WatchSpec{
		Schedule: r.Schedule, Pipeline: r.Pipeline, Enabled: r.Enabled,
		Compiled: compiled.Compiled, LoweredPlan: lowered,
	}
	if err := s.Crons.SaveWatch(r.Owner, r.Home, r.User, r.Name, spec); err != nil {
		return err
	}
	return Lint(r.Schedule, r.Pipeline, lowered)
}

// binResolver is how the planner reads a /bin script when it lowers a pipeline.
type binResolver struct {
	bins  bin.Store
	owner shape.Added
}

func (r binResolver) Source(name string) (string, string, bool) {
	fn, err := r.bins.Get(r.owner, name)
	if err != nil {
		return "", "", false
	}
	return fn.Source, planner.HashBinSource(fn.Source), true
}

// checkValueTypes refuses a save that would put a text cron behind a numeric
// reader: this pipeline reading a text cron with a numeric weft:: function, or
// this cron turning text while numeric readers name it.
func (s Service) checkValueTypes(owner shape.Added, ns uint, name string, c Compiled) error {
	// References name a cron by <namespace>/<name>, so this cron is "self" only
	// when it has a namespace. An owner-level cron can't be referenced.
	self := ""
	if ns != 0 {
		nsName, ok, err := s.Sources.NamespaceName(owner, ns)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no namespace with id %d", ns)
		}
		self = nsName + "/" + name
	}
	for _, ref := range c.NumericRefs {
		if self != "" && ref == self && c.ValueType == cron.ValueText {
			return fmt.Errorf("this cron stores text (# type: text), so it can't read itself with a numeric weft:: function")
		}
	}
	refs := binscript.Service{Crons: s.Crons, Sources: s.Sources}
	if err := refs.RefuseTextRefs(owner, c.NumericRefs, self); err != nil {
		return err
	}
	if c.ValueType != cron.ValueText || self == "" {
		return nil
	}
	if existing, err := s.Crons.GetWatch(owner, ns, name); err == nil && existing.Compiled.ValueType == cron.ValueText {
		return nil // already text: its readers were checked when they were saved
	}
	readers, err := s.numericReaders(owner, self)
	if err != nil {
		return err
	}
	if len(readers) > 0 {
		return fmt.Errorf("can't make %q a text cron while numeric readers use it: %s", self, strings.Join(readers, ", "))
	}
	return nil
}

// numericReaders lists what reads the cron at path (<namespace>/<name>) as
// numbers: other crons' and /bin scripts' literal numeric weft calls (bash or
// Lua), and whatever Readers adds.
func (s Service) numericReaders(owner shape.Added, path string) ([]string, error) {
	var out []string
	reads := func(refs []string) bool {
		for _, ref := range refs {
			if ref == path {
				return true
			}
		}
		return false
	}
	scripts, err := s.Bins.List(owner)
	if err != nil {
		return nil, err
	}
	for _, fn := range scripts {
		if refs, err := binscript.Refs(fn.Language, fn.Source); err == nil && reads(refs) {
			out = append(out, "/bin/"+fn.Name)
		}
	}
	crons, err := s.Crons.ListWatches(owner)
	if err != nil {
		return nil, err
	}
	nss, err := s.Sources.ListNamespaces(owner)
	if err != nil {
		return nil, err
	}
	nsNames := map[uint]string{}
	for _, n := range nss {
		nsNames[n.ID] = n.Name
	}
	for _, c := range crons {
		cpath := c.Name
		if c.Namespace != 0 {
			cpath = nsNames[c.Namespace] + "/" + c.Name
		}
		if cpath == path {
			continue
		}
		if compiled, err := Compile(c.Pipeline); err == nil && reads(compiled.NumericRefs) {
			out = append(out, "/cron.d/"+cpath)
		}
	}
	if s.Readers != nil {
		more, err := s.Readers(owner, path)
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	sort.Strings(out)
	return out, nil
}
