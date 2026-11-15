// Package ingeststoretest is the conformance suite for ingestfail.Store: the
// behaviour every implementation of the ingestion-failure domain (Postgres for
// the hosted build, SQLite for the open one) must show.
package ingeststoretest

import (
	"slices"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/ingestfail"
	"github.com/w3ft-net/weft/shape"
)

// Subject is one store under test, fresh and empty.
type Subject struct {
	Store ingestfail.Store
	// Owner owns the failures the suite records. The open build's is the empty
	// value.
	Owner shape.Added
	// Other is a second owner, for a store that has owners to tell apart. Nil for
	// one that has none.
	Other *shape.Added
}

// Run runs the suite, building a fresh Subject for each test.
func Run(t *testing.T, newSubject func(t *testing.T) Subject) {
	cases := []struct {
		name string
		fn   func(*testing.T, Subject)
	}{
		{"NoFailuresIsAnEmptyListNotAnError", none},
		{"ServicesAreDistinctAndSorted", distinct},
		{"OnlyFailuresSinceTheCutoffCount", window},
		{"OwnersAreSeparate", owners},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newSubject(t)) })
	}
}

func record(t *testing.T, s Subject, owner shape.Added, at time.Time, services ...string) {
	t.Helper()
	err := s.Store.Record(ingestfail.Failure{
		Owner: owner, At: at, ShardID: "shard-1", Message: "disk full", Rows: 10, Services: services, DLQPath: "dlq/1",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func affected(t *testing.T, s Subject, owner shape.Added, since time.Time) []string {
	t.Helper()
	got, err := s.Store.AffectedServices(owner, since)
	if err != nil {
		t.Fatalf("AffectedServices: %v", err)
	}
	if got == nil {
		t.Fatal("AffectedServices returned nil, want a (possibly empty) slice")
	}
	return got
}

func none(t *testing.T, s Subject) {
	if got := affected(t, s, s.Owner, time.Now().Add(-time.Hour)); len(got) != 0 {
		t.Fatalf("affected = %v, want none", got)
	}
}

func distinct(t *testing.T, s Subject) {
	now := time.Now()
	record(t, s, s.Owner, now, "ops/web/main", "ops/api/main")
	record(t, s, s.Owner, now, "ops/web/main", "ops/batch/main")
	want := []string{"ops/api/main", "ops/batch/main", "ops/web/main"}
	if got := affected(t, s, s.Owner, now.Add(-time.Minute)); !slices.Equal(got, want) {
		t.Fatalf("affected = %v, want %v", got, want)
	}
}

func window(t *testing.T, s Subject) {
	now := time.Now()
	record(t, s, s.Owner, now.Add(-3*time.Hour), "ops/old/main")
	record(t, s, s.Owner, now.Add(-10*time.Minute), "ops/new/main")
	if got := affected(t, s, s.Owner, now.Add(-time.Hour)); !slices.Equal(got, []string{"ops/new/main"}) {
		t.Fatalf("affected = %v, want only the recent failure", got)
	}
}

func owners(t *testing.T, s Subject) {
	if s.Other == nil {
		t.Skip("one implicit owner")
	}
	now := time.Now()
	record(t, s, s.Owner, now, "ops/mine/main")
	record(t, s, *s.Other, now, "ops/theirs/main")
	if got := affected(t, s, s.Owner, now.Add(-time.Minute)); !slices.Equal(got, []string{"ops/mine/main"}) {
		t.Fatalf("owner sees %v, want only its own", got)
	}
	if got := affected(t, s, *s.Other, now.Add(-time.Minute)); !slices.Equal(got, []string{"ops/theirs/main"}) {
		t.Fatalf("other owner sees %v, want only its own", got)
	}
}
