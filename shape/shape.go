// Package shape is the coordinate vocabulary of the hosted build: the columns
// that locate a log row, and sets of tuples over them. It is hand-written for
// now; docs/plans/tenant-scope.md Phase 4 turns it into generator output.
package shape

import (
	"errors"
	"slices"
)

type cellKind uint8

const (
	kindAny cellKind = iota
	kindNull
	kindValue
)

// Cell constrains one non-required column of a tuple: any value, a missing
// value (stored as ”), or exactly one value. The zero Cell is Any.
type Cell struct {
	kind cellKind
	val  string
}

func Any() Cell            { return Cell{} }
func Null() Cell           { return Cell{kind: kindNull} }
func Value(v string) Cell  { return Cell{kind: kindValue, val: v} }
func (c Cell) IsAny() bool { return c.kind == kindAny }

// Matches reports whether a row's column value falls inside the cell.
func (c Cell) Matches(v string) bool {
	switch c.kind {
	case kindNull:
		return v == ""
	case kindValue:
		return v == c.val
	}
	return true
}

// Coord is one tuple. Customer is a required column: it is always a concrete
// value, never a wildcard, and 0 is not a customer.
type Coord struct {
	Customer                      int64
	Namespace, Env, Service, Kind Cell
}

// Set is a union of tuples. The empty set matches nothing.
type Set []Coord

// ErrNoCustomer is returned for a tuple whose required Customer column is unset.
var ErrNoCustomer = errors.New("shape: tuple has no customer")

// Validate refuses a set that leaves a required column unset.
func (s Set) Validate() error {
	for _, c := range s {
		if c.Customer == 0 {
			return ErrNoCustomer
		}
	}
	return nil
}

// ErrFinerThanNamespace is returned when a tuple narrows below the namespace.
// Callers that can only enforce namespaces must refuse such a set: dropping
// the narrowing would widen the grant.
var ErrFinerThanNamespace = errors.New("shape: scope narrower than a namespace is not supported here")

// Namespaces reports the namespaces the set grants within one customer: all
// is true when some tuple leaves the namespace unconstrained, otherwise names
// is the sorted, distinct set of named namespaces. A tuple that also
// constrains env, service or kind is ErrFinerThanNamespace.
func (s Set) Namespaces(customer int64) (names []string, all bool, err error) {
	for _, c := range s {
		if c.Customer != customer {
			continue
		}
		if !c.Env.IsAny() || !c.Service.IsAny() || !c.Kind.IsAny() {
			return nil, false, ErrFinerThanNamespace
		}
		switch {
		case c.Namespace.IsAny():
			all = true
		case c.Namespace.kind == kindValue:
			names = append(names, c.Namespace.val)
		}
	}
	slices.Sort(names)
	return slices.Compact(names), all, nil
}
