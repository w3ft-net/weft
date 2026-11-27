package mem

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sources"
)

// Sources is a source.Store in memory: namespaces and the sources in them. The
// token and shard stores built over it share its namespaces.
type Sources struct {
	mu                sync.Mutex
	producers         *sources.Registry
	nextNS            uint
	nextSrc           uint
	namespaces        []*source.Namespace
	rows              []*source.Source // by (NamespaceID, Service)
	onDeleteNamespace []func(id uint)
}

var _ source.Store = (*Sources)(nil)

// NewSources is an empty store over the core's source kinds.
func NewSources() *Sources { return &Sources{producers: sources.Core(nil)} }

// WithProducers installs the registry of source kinds; the default is sources.Core.
func (s *Sources) WithProducers(r *sources.Registry) *Sources {
	s.producers = r
	return s
}

func (s *Sources) nsByName(name string) *source.Namespace {
	for _, n := range s.namespaces {
		if n.Name == name {
			return n
		}
	}
	return nil
}

func (s *Sources) nsName(id uint) (string, bool) {
	for _, n := range s.namespaces {
		if n.ID == id {
			return n.Name, true
		}
	}
	return "", false
}

func (s *Sources) find(nsID uint, service string) *source.Source {
	for _, r := range s.rows {
		if r.NamespaceID == nsID && r.Service == service {
			return r
		}
	}
	return nil
}

func (s *Sources) insert(nsID uint, kind, service string, status sources.Status, spec source.SourceSpec, schemaYAML string, resample bool) {
	name, _ := s.nsName(nsID)
	s.nextSrc++
	now := time.Now().UTC()
	r := &source.Source{ID: s.nextSrc, NamespaceID: nsID, Namespace: name, Service: service, Kind: kind, Status: status,
		Locator: spec.Locator, Params: copyParams(spec.Params), SchemaYAML: schemaYAML, Enabled: true, CreatedAt: now}
	if resample {
		r.ResampleRequestedAt = &now
	}
	s.rows = append(s.rows, r)
}

func copyParams(p map[string]string) map[string]string {
	if len(p) == 0 {
		return nil
	}
	out := make(map[string]string, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

func cp(r *source.Source) *source.Source {
	c := *r
	c.Params = copyParams(r.Params)
	return &c
}

func (s *Sources) FindOrCreateNamespace(_ shape.Added, name string) (*source.Namespace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.nsByName(name); n != nil {
		c := *n
		return &c, nil
	}
	s.nextNS++
	n := &source.Namespace{ID: s.nextNS, Name: name}
	s.namespaces = append(s.namespaces, n)
	c := *n
	return &c, nil
}

func (s *Sources) GetNamespace(_ shape.Added, name string) (*source.Namespace, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.nsByName(name); n != nil {
		c := *n
		return &c, true, nil
	}
	return nil, false, nil
}

func (s *Sources) NamespaceName(_ shape.Added, id uint) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.nsName(id)
	return name, ok, nil
}

func (s *Sources) ListNamespaces(shape.Added) ([]source.Namespace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []source.Namespace
	for _, n := range s.namespaces {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Sources) DeleteNamespace(_ context.Context, _ shape.Added, name string) error {
	s.mu.Lock()
	n := s.nsByName(name)
	if n == nil {
		s.mu.Unlock()
		return source.ErrNotFound
	}
	id := n.ID
	kept := s.rows[:0]
	for _, r := range s.rows {
		if r.NamespaceID != id {
			kept = append(kept, r)
		}
	}
	s.rows = kept
	ns := s.namespaces[:0]
	for _, x := range s.namespaces {
		if x.ID != id {
			ns = append(ns, x)
		}
	}
	s.namespaces = ns
	hooks := append([]func(uint){}, s.onDeleteNamespace...)
	s.mu.Unlock()
	for _, h := range hooks {
		h(id)
	}
	return nil
}

func (s *Sources) AddSource(_ context.Context, nsID uint, kind, service string, spec source.SourceSpec, schemaYAML string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.find(nsID, service); r != nil {
		r.Kind, r.Status, r.Locator, r.Params, r.SchemaYAML, r.Sample, r.Detection =
			kind, sources.Configured, spec.Locator, copyParams(spec.Params), schemaYAML, "", ""
		return nil
	}
	s.insert(nsID, kind, service, sources.Configured, spec, schemaYAML, false)
	return nil
}

func (s *Sources) RegisterSource(nsID uint, kind, service string, spec source.SourceSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.find(nsID, service) != nil {
		return nil
	}
	start := s.producers.Producer(kind).InitialState(sources.Registration{Locator: spec.Locator, Params: spec.Params})
	s.insert(nsID, kind, service, start.Status, spec, "", start.ResampleRequested)
	return nil
}

func (s *Sources) EnsureSource(nsID uint, service string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.find(nsID, service) != nil {
		return nil
	}
	start := s.producers.Producer("direct").InitialState(sources.Registration{Locator: service})
	s.insert(nsID, "direct", service, start.Status, source.SourceSpec{Locator: service}, "", start.ResampleRequested)
	return nil
}

func (s *Sources) DeleteSource(nsID uint, service string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.rows[:0]
	for _, r := range s.rows {
		if !(r.NamespaceID == nsID && r.Service == service) {
			kept = append(kept, r)
		}
	}
	s.rows = kept
	return nil
}

func (s *Sources) ConfigureSource(_ context.Context, nsID uint, service, schemaYAML string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.find(nsID, service)
	if r == nil || (r.Status != sources.Pending && r.Status != sources.Configured) {
		return source.ErrNotFound
	}
	r.Status, r.SchemaYAML, r.Sample, r.Detection, r.SampleCollected, r.ResampleRequestedAt =
		sources.Configured, schemaYAML, "", "", 0, nil
	return nil
}

func (s *Sources) FindSource(_ shape.Added, namespace, service string) (*source.Source, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nsByName(namespace)
	if n == nil {
		return nil, nil
	}
	if r := s.find(n.ID, service); r != nil {
		return cp(r), nil
	}
	return nil, nil
}

func (s *Sources) SourceSchema(ctx context.Context, owner shape.Added, namespace, service string) (string, bool, error) {
	src, err := s.FindSource(owner, namespace, service)
	if err != nil || src == nil {
		return "", false, err
	}
	if src.Status != sources.Configured || src.SchemaYAML == "" {
		return "", false, nil
	}
	return src.SchemaYAML, true, nil
}

func (s *Sources) FindServiceByLocator(_ shape.Added, namespace, locator string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nsByName(namespace)
	if n == nil {
		return "", false, nil
	}
	for _, r := range s.rows {
		if r.NamespaceID == n.ID && r.Locator == locator {
			return r.Service, true, nil
		}
	}
	return "", false, nil
}

// awaitingSchema is the predicate for sources a person still has to give a
// schema: those sampling or pending, plus configured ones collecting or holding
// a sample for review.
func awaitingSchema(r *source.Source) bool {
	if r.Status == sources.Sampling || r.Status == sources.Pending {
		return true
	}
	if r.Status != sources.Configured {
		return false
	}
	if r.SchemaYAML == "" {
		return r.SampleCollected > 0 || r.SampleUpdatedAt != nil
	}
	return r.ResampleRequestedAt != nil || (r.Sample != "" && r.Sample != "[]" && r.Sample != "null")
}

func (s *Sources) ListSources(_ shape.Added, f source.Filter) ([]source.Source, error) {
	if f.Namespaces != nil && len(f.Namespaces) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []source.Source
	for _, r := range s.rows {
		if f.Namespace != "" && r.Namespace != f.Namespace {
			continue
		}
		if f.Namespaces != nil {
			found := false
			for _, n := range f.Namespaces {
				found = found || n == r.Namespace
			}
			if !found {
				continue
			}
		}
		if f.AwaitingSchema && !awaitingSchema(r) {
			continue
		}
		out = append(out, *cp(r))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Service < out[j].Service
	})
	return out, nil
}

func (s *Sources) SubmitSample(nsID uint, service string, sub source.SampleSubmission) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	status := s.producers.Producer("weftd").InitialState(sources.Registration{Partial: sub.Partial}).Status
	r := s.find(nsID, service)
	if r == nil {
		s.insert(nsID, "weftd", service, status, source.SourceSpec{Locator: service}, "", false)
		r = s.find(nsID, service)
		r.Instance, r.Deployment, r.Sample, r.Detection, r.SampleCollected, r.SampleUpdatedAt = sub.Instance, sub.Deployment, sub.SampleJSON, sub.DetectionJSON, sub.Collected, &now
		return nil
	}
	if r.Status == sources.Configured {
		if r.ResampleRequestedAt == nil {
			return nil // a stale sample: the schema is approved
		}
		status = sources.Configured
	}
	r.Status, r.Instance, r.Deployment, r.Sample, r.Detection, r.SampleCollected, r.SampleUpdatedAt =
		status, sub.Instance, sub.Deployment, sub.SampleJSON, sub.DetectionJSON, sub.Collected, &now
	if !sub.Partial {
		r.ResampleRequestedAt = nil
	}
	return nil
}

func (s *Sources) RequestResample(nsID uint, service string, size int) error {
	if size == 0 {
		size = source.SampleTarget
	}
	if size < source.SampleTarget || size > source.MaxSampleTarget {
		return source.ErrBadSampleSize
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.find(nsID, service)
	if r == nil {
		return source.ErrNotFound
	}
	if r.Status == sources.Sampling || (r.Status == sources.Configured && r.ResampleRequestedAt != nil) {
		return source.ErrAlreadySampling
	}
	plan := s.producers.Producer(r.Kind).Resample(r.View())
	now := time.Now().UTC()
	r.ResampleRequestedAt, r.SampleSize = &now, size
	if plan.Status != "" {
		r.Status = plan.Status
	}
	if plan.ResetSample {
		r.Sample, r.Detection, r.SampleCollected, r.SampleUpdatedAt = "", "", 0, &now
	}
	return nil
}

func (s *Sources) UpdateDetection(nsID uint, service, sampleJSON, detectionJSON string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.find(nsID, service); r != nil && r.Sample == sampleJSON {
		r.Detection = detectionJSON
	}
	return nil
}

func (s *Sources) ServicesInBucket(bucketID int64, unassigned bool) ([]shardstore.ShardService, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []shardstore.ShardService
	for _, r := range s.rows {
		if (r.BucketID != nil && *r.BucketID == bucketID) || (unassigned && r.BucketID == nil) {
			out = append(out, shardstore.ShardService{NamespaceID: r.NamespaceID, Service: r.Service})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NamespaceID != out[j].NamespaceID {
			return out[i].NamespaceID < out[j].NamespaceID
		}
		return out[i].Service < out[j].Service
	})
	return out, nil
}
