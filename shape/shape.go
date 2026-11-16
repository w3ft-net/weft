// Package shape is the coordinate vocabulary of a build: the columns that locate
// a log row, and sets of tuples over them.
//
// Which columns a build has is chosen when the coordinator is compiled, from a
// spec (shape/specs). The generator writes coord_gen.go from it: the tuple
// (Coord), the key a row is matched by (Key), and the few operations that name
// columns. This file is the algebra over them, and names none: it is the same
// in every build.
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

// Set is a union of tuples. The empty set matches nothing.
type Set []Coord

// ErrRequiredUnset is returned for a tuple that leaves a required column unset:
// a required column is never a wildcard.
var ErrRequiredUnset = errors.New("shape: tuple leaves a required column unset")

// ErrNoScope is returned by Partition for an empty set: it names no partition,
// and an empty set reads nothing.
var ErrNoScope = errors.New("shape: an empty set names no partition")

// ErrMixedPartitions is returned by Partition for a set whose tuples name more
// than one partition. A caller that serves one group at a time refuses it
// rather than picking one.
var ErrMixedPartitions = errors.New("shape: the set names more than one partition")

// Validate refuses a set that leaves a required column unset.
func (s Set) Validate() error {
	for i := range s {
		if err := s[i].validate(); err != nil {
			return err
		}
	}
	return nil
}

// ErrFinerThanNamespace is returned when a tuple narrows below the namespace.
// Callers that can only enforce namespaces must refuse such a set: dropping
// the narrowing would widen the grant.
var ErrFinerThanNamespace = errors.New("shape: scope narrower than a namespace is not supported here")

// Intersect is what a query may read: the tuples that satisfy both the rows it
// names (source) and the rows its principal may see (scope). It is a union of
// pairwise intersections, so the source's tuples stay tuples: `a/api` plus
// `b/web` never admits `a/web`. An empty result is a deny, decided before
// routing and indistinguishable from a source that does not exist.
func Intersect(source, scope Set) Set {
	var out Set
	for i := range source {
		for j := range scope {
			if c, ok := source[i].meet(scope[j]); ok && !slices.Contains(out, c) {
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
