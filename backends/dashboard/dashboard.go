// Package dashboard is the contract of the dashboard domain: the YAML bodies a
// group keeps under /etc/dashboards. Postgres implements it for the hosted build
// and SQLite for the open one (see defaults and dashboard/dashboardstoretest).
//
// The contract holds what is stored, not what a body may say: whether it parses
// (package dashboard, in the module root) and whether the crons it names exist
// and may be read are the callers' to judge before they save.
//
// It imports only the standard library and shape.
package dashboard

import (
	"errors"
	"time"

	"github.com/w3ft-net/weft/shape"
)

// ErrNotFound is the answer for a dashboard the caller named that does not
// exist.
var ErrNotFound = errors.New("dashboard not found")

// Dashboard is a stored dashboard. YAMLBody is the authoritative form: exactly
// what the user wrote.
type Dashboard struct {
	Owner    shape.Added
	Name     string
	YAMLBody string
	// CreatedBy is the user who first saved the dashboard, "" for none; an edit
	// keeps it.
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store is the persistence contract of the dashboard domain. Every operation is
// confined to its owner's dashboards.
type Store interface {
	// Save writes the dashboard: a new one, or an edit that keeps its creator and
	// moves UpdatedAt. Saving the body a dashboard already has is a no-op, so that
	// an unchanged buffer written back leaves its time alone.
	Save(owner shape.Added, user, name, yamlBody string) error
	// Get is the dashboard, ErrNotFound if there is none.
	Get(owner shape.Added, name string) (*Dashboard, error)
	// List is the owner's dashboards by name.
	List(owner shape.Added) ([]Dashboard, error)
	// Delete removes a dashboard; a missing one is not an error. Saving the name
	// again makes a new dashboard, with a new creator.
	Delete(owner shape.Added, name string) error
}
