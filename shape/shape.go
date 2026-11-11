// Package shape is the coordinate vocabulary of the hosted build: the columns
// that locate a log row, and sets of tuples over them. It is hand-written for
// now; docs/plans/tenant-scope.md Phase 4 turns it into generator output.
package shape

import (
	"encoding/json"
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

// Literal is the one value a Value cell names.
func (c Cell) Literal() (string, bool) { return c.val, c.kind == kindValue }

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

// meet is the intersection of two cells; ok is false when nothing satisfies both.
func (c Cell) meet(o Cell) (Cell, bool) {
	switch {
	case c.kind == kindAny:
		return o, true
	case o.kind == kindAny:
		return c, true
	case c.kind == kindNull && o.kind == kindNull:
		return c, true
	case c.kind == kindValue && o.kind == kindValue && c.val == o.val:
		return c, true
	}
	return Cell{}, false
}

// Intersect is what a query may read: the tuples that satisfy both the rows it
// names (source) and the rows its principal may see (scope). It is a union of
// pairwise intersections, so the source's tuples stay tuples: `a/api` plus
// `b/web` never admits `a/web`. An empty result is a deny, decided before
// routing and indistinguishable from a source that does not exist.
func Intersect(source, scope Set) Set {
	var out Set
	for _, s := range source {
		for _, p := range scope {
			if s.Customer != p.Customer {
				continue
			}
			c := Coord{Customer: s.Customer}
			var ok [4]bool
			c.Namespace, ok[0] = s.Namespace.meet(p.Namespace)
			c.Env, ok[1] = s.Env.meet(p.Env)
			c.Service, ok[2] = s.Service.meet(p.Service)
			c.Kind, ok[3] = s.Kind.meet(p.Kind)
			if ok[0] && ok[1] && ok[2] && ok[3] && !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
	}
	return out
}

// cellJSON is a Cell's stored form: Any is {}, so a tuple that constrains
// nothing stays small.
type cellJSON struct {
	Kind string `json:"k,omitempty"`
	Val  string `json:"v,omitempty"`
}

func (c Cell) MarshalJSON() ([]byte, error) {
	switch c.kind {
	case kindNull:
		return json.Marshal(cellJSON{Kind: "null"})
	case kindValue:
		return json.Marshal(cellJSON{Kind: "value", Val: c.val})
	}
	return json.Marshal(cellJSON{})
}

func (c *Cell) UnmarshalJSON(b []byte) error {
	var j cellJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	switch j.Kind {
	case "":
		*c = Any()
	case "null":
		*c = Null()
	case "value":
		*c = Value(j.Val)
	default:
		return errors.New("shape: unknown cell kind " + j.Kind)
	}
	return nil
}

// Covers reports whether scope admits every row want names: intersecting want
// with scope loses nothing. It is deliberately conservative: a scope that
// covers want only by splitting it across several tuples reads as not covering,
// so a caller that skips a re-plan on Covers never skips one it needed.
func Covers(scope, want Set) bool {
	if len(want) == 0 {
		return false
	}
	got := Intersect(want, scope)
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			return false
		}
	}
	return true
}

// Matches reports whether a row's coordinates fall inside the tuple. The live
// matcher and the SQL backend must agree on this for every row.
func (c Coord) Matches(customer int64, namespace, env, service, kind string) bool {
	return c.Customer == customer &&
		c.Namespace.Matches(namespace) &&
		c.Env.Matches(env) &&
		c.Service.Matches(service) &&
		c.Kind.Matches(kind)
}

// Matches reports whether any tuple admits the row.
func (s Set) Matches(customer int64, namespace, env, service, kind string) bool {
	for _, c := range s {
		if c.Matches(customer, namespace, env, service, kind) {
			return true
		}
	}
	return false
}

// Shared is the one value every tuple pins in a column, or "" and false when
// some tuple leaves the column open or they differ. A superset backend such as
// shard routing narrows only by a shared value.
func Shared(s Set, col func(Coord) Cell) (string, bool) {
	var v string
	for i, c := range s {
		lit, ok := col(c).Literal()
		if !ok || (i > 0 && lit != v) {
			return "", false
		}
		v = lit
	}
	return v, len(s) > 0
}

// Overlaps reports whether the set admits any row the tuple names: the tuple
// intersected with the set is not empty. A source is a coordinate (a namespace
// and service, across every kind and env), so this is what makes it visible.
func (s Set) Overlaps(c Coord) bool { return len(Intersect(Set{c}, s)) > 0 }

// NamespaceSuperset is the namespaces some tuple of the customer touches, and
// whether some tuple leaves the namespace open. Unlike Namespaces it accepts a
// scope narrower than a namespace, and the answer is a superset: it is for
// narrowing a lookup that is then filtered exactly, never for granting.
func (s Set) NamespaceSuperset(customer int64) (names []string, all bool) {
	for _, c := range s {
		if c.Customer != customer {
			continue
		}
		if v, ok := c.Namespace.Literal(); ok {
			names = append(names, v)
		} else {
			all = true
		}
	}
	slices.Sort(names)
	return slices.Compact(names), all
}
