// Package ingestfail is the contract of the ingestion-failure domain: the
// record of batches of rows the coordinator could not store, kept so that a
// tenant can be told which of its services are affected. Postgres implements it
// for the hosted build and SQLite for the open one (see defaults and
// ingestfail/ingeststoretest).
//
// A failed batch belongs to one owner, so what is recorded and what is asked for
// is per owner: a tenant is never told about another's services.
//
// It imports only the standard library and shape.
package ingestfail

import (
	"time"

	"github.com/w3ft-net/weft/shape"
)

// Failure is one batch of rows that could not be stored.
type Failure struct {
	Owner shape.Added
	// At is when it failed; zero is now.
	At      time.Time
	ShardID string
	Message string
	// Rows is how many rows the batch held.
	Rows int
	// Services are the services those rows belong to, each as the shell names
	// them (<namespace>/<service>/<kind>).
	Services []string
	// DLQPath is where the batch went instead, "" if nowhere.
	DLQPath string
}

// Store is the persistence contract of the ingestion-failure domain.
type Store interface {
	// Record adds a failure.
	Record(f Failure) error
	// AffectedServices are the distinct services the owner has had failures for
	// since the given time, sorted. A tenant that has had none gets an empty
	// slice, not an error.
	AffectedServices(owner shape.Added, since time.Time) ([]string, error)
}
