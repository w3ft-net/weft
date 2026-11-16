// Package source is the contract of the namespaces-and-sources domain: the
// namespaces a build's log sources live under, the sources themselves, and the
// Store interface every build implements (Postgres for the hosted build, SQLite
// for the open one; see defaults and source/sourcestoretest).
//
// Whose namespace it is, is said in the build's own coordinates, a shape.Added.
// The default shape adds no column, so in the open build the owner is the empty
// value and the store has no org, no group column and no seeded row; the hosted
// shape adds the customer, which the Postgres store turns into its org handle.
// Nothing here names an org. What differs between kinds of source is the
// business of package sources.
//
// The package imports only the standard library and this module's cgo-free
// leaves, so a process that links an implementation stays cgo-free.
package source

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sources"
)

// SampleTarget is the line count a full sample carries unless a resample asked
// for more (Source.SampleSize). The producer owns the real accumulation; this
// is the canonical value the coordinator and UIs report progress against.
const SampleTarget = 50

// MaxSampleTarget bounds a resample's requested size: the sample is stored on
// the source row and handed to the classifier and the wizard whole.
const MaxSampleTarget = 2000

var (
	// ErrNotFound is the answer for a source the caller named that does not
	// exist.
	ErrNotFound = errors.New("source not found")
	// ErrBadSampleSize is RequestResample's answer for a size outside
	// [SampleTarget, MaxSampleTarget].
	ErrBadSampleSize = fmt.Errorf("sample size must be between %d and %d lines", SampleTarget, MaxSampleTarget)
	// ErrAlreadySampling is RequestResample's answer for a source that is
	// already collecting a sample, its first or a resample.
	ErrAlreadySampling = errors.New("a sample is already being collected")
)

// Namespace is a grouping of sources: one path segment (/log/<ns>/...), the
// scope of a token, and the stem of its permission groups.
type Namespace struct {
	ID   uint
	Name string
}

// Source is a log source, pending or configured.
type Source struct {
	ID          uint
	NamespaceID uint
	// Namespace is the owning namespace's name; reads fill it.
	Namespace string
	Service   string
	// BucketID assigns the service to a bucket; nil is the default bucket.
	BucketID *int64
	// Kind is the registered name of the producer (package sources).
	Kind   string
	Status sources.Status
	// Locator and Params are the producer's address for the source and its other
	// parameters (a Firehose stream's log group, a region).
	Locator    string
	Params     map[string]string
	SchemaYAML string
	Enabled    bool
	// Instance and Deployment name the agent that last submitted a sample, for a
	// source an agent samples.
	Instance   string
	Deployment string
	// Sample is a JSON array of raw lines and Detection the JSON-encoded
	// detection over it; both are cleared when a schema is configured.
	Sample    string
	Detection string
	// SampleCollected is the accumulation progress while the first sample fills.
	SampleCollected int
	// SampleUpdatedAt is when the sample last changed, by the store's clock.
	SampleUpdatedAt *time.Time
	// ResampleRequestedAt is set while a fresh sample of a configured source is
	// being collected.
	ResampleRequestedAt *time.Time
	// SampleSize is the line count the current sample collects toward, when a
	// resample asked for more than SampleTarget; zero means the default.
	SampleSize int
	// CreatedAt is when the source was first recorded.
	CreatedAt time.Time
}

// Target is the line count a full sample of the source carries.
func (s Source) Target() int {
	if s.SampleSize > 0 {
		return s.SampleSize
	}
	return SampleTarget
}

// View is the part of the row a producer's rules read.
func (s Source) View() sources.Source {
	return sources.Source{ID: s.ID, Status: s.Status, ResampleRequested: s.ResampleRequestedAt != nil}
}

// SourceSpec is how a source is addressed: where its producer finds it.
type SourceSpec struct {
	Locator string
	Params  map[string]string
}

// SampleSubmission is one submission of a sample by an agent.
type SampleSubmission struct {
	Instance      string
	Deployment    string
	SampleJSON    string
	DetectionJSON string
	// Collected is the running line count.
	Collected int
	// Partial is set while the agent is still accumulating (a progress ping);
	// the final, complete sample has it unset.
	Partial bool
}

// Filter selects sources.
type Filter struct {
	// Namespace narrows to one namespace by name; empty is every namespace.
	Namespace string
	// Namespaces narrows to the named namespaces; nil is every namespace.
	Namespaces []string
	// AwaitingSchema narrows to the sources a person still has to give a schema:
	// those sampling or pending, plus configured ones collecting or holding a
	// sample for review.
	AwaitingSchema bool
}

// Store is the persistence contract of the namespaces-and-sources domain.
//
// A source is unique by (namespace, service). A namespace is unique by name
// within its owner.
type Store interface {
	// FindOrCreateNamespace returns the owner's namespace of that name, creating
	// it if need be.
	FindOrCreateNamespace(owner shape.Added, name string) (*Namespace, error)
	// GetNamespace is (ns, true, nil) when found, (nil, false, nil) when absent.
	GetNamespace(owner shape.Added, name string) (*Namespace, bool, error)
	// NamespaceName is the name of the namespace with this id.
	NamespaceName(owner shape.Added, id uint) (string, bool, error)
	ListNamespaces(owner shape.Added) ([]Namespace, error)
	// DeleteNamespace removes a namespace and its sources for good, so its name
	// can be created again. A missing namespace is ErrNotFound.
	DeleteNamespace(ctx context.Context, owner shape.Added, name string) error

	// AddSource creates a configured source, or reconfigures an existing one of
	// the same (namespace, service): it takes the kind and address given, the
	// schema, and an empty sample.
	AddSource(ctx context.Context, namespaceID uint, kind, service string, spec SourceSpec, schemaYAML string) error
	// RegisterSource creates a source before any data arrives, in the state its
	// producer starts a new source in. It is a no-op if the source exists.
	RegisterSource(namespaceID uint, kind, service string, spec SourceSpec) error
	// EnsureSource is the auto-onboard path for a producer that never announces
	// itself: it records that the service exists as a direct source, a no-op if
	// any source of that name does.
	EnsureSource(namespaceID uint, service string) error
	// DeleteSource removes one source for good: a source of that name that is gone
	// is not an error, and the namespace stays. Its sample and agents go with it.
	DeleteSource(namespaceID uint, service string) error
	// ConfigureSource lands a schema on an existing pending or configured source,
	// clears its sample, detection and progress, ends any resample, and makes it
	// configured. A source that is missing, or still sampling, is ErrNotFound.
	ConfigureSource(ctx context.Context, namespaceID uint, service, schemaYAML string) error

	// FindSource is (nil, nil) when there is no such source.
	FindSource(owner shape.Added, namespace, service string) (*Source, error)
	// SourceSchema is the source's configured schema for ingest-time
	// enrichment; ok is false when it has none (never onboarded, still pending,
	// or empty), which is not an error.
	SourceSchema(ctx context.Context, owner shape.Added, namespace, service string) (schemaYAML string, ok bool, err error)
	// FindServiceByLocator resolves a producer's address for a source to its
	// service, ok false when no source of the namespace has it.
	FindServiceByLocator(owner shape.Added, namespace, locator string) (service string, ok bool, err error)
	// ListSources returns the owner's sources, ordered by namespace and service.
	ListSources(owner shape.Added, f Filter) ([]Source, error)

	// SubmitSample records an agent's sample for (namespace, service). A source
	// that does not exist is created in the state weftd's producer starts one in.
	// One that is configured with no resample in flight ignores a stale sample.
	SubmitSample(namespaceID uint, service string, sub SampleSubmission) error
	// RequestResample asks for a fresh sample of size lines (0 is SampleTarget).
	// ErrNotFound, ErrBadSampleSize and ErrAlreadySampling are its refusals;
	// what else it does to the source is its producer's rule.
	RequestResample(namespaceID uint, service string, size int) error
	// UpdateDetection writes a freshly classified detection, only while
	// sampleJSON is still the source's sample.
	UpdateDetection(namespaceID uint, service, sampleJSON, detectionJSON string) error

	// ServicesInBucket returns the (namespace, service) pairs assigned to a
	// bucket. unassigned adds the sources assigned to none, which belong to the
	// default bucket; the caller knows which bucket that is, because buckets are
	// not this domain's.
	ServicesInBucket(bucketID int64, unassigned bool) ([]shardstore.ShardService, error)
}
