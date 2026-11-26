// Package dashboardstoretest is the conformance suite for dashboard.Store: the
// behaviour every implementation of the dashboard domain (Postgres for the
// hosted build, SQLite for the open one) must show. An implementation's test
// calls Run with a constructor.
package dashboardstoretest

import (
	"errors"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/dashboard"
	"github.com/w3ft-net/weft/shape"
)

// Subject is one store under test, fresh and empty.
type Subject struct {
	Store dashboard.Store
	// Owner owns the dashboards the suite makes. The open build's is the empty
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
		{"SaveAndGet", saveAndGet},
		{"GetMissing", getMissing},
		{"UnchangedSaveIsANoOp", unchangedSave},
		{"EditKeepsTheCreator", editKeepsCreator},
		{"ListIsByName", list},
		{"DeleteThenSaveMakesANewOne", deleteThenSave},
		{"DeleteMissingIsNotAnError", deleteMissing},
		{"OwnersAreSeparate", owners},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newSubject(t)) })
	}
}

func save(t *testing.T, s Subject, user, name, body string) {
	t.Helper()
	if err := s.Store.Save(s.Owner, user, name, body); err != nil {
		t.Fatalf("Save %q: %v", name, err)
	}
}

func get(t *testing.T, s Subject, name string) dashboard.Dashboard {
	t.Helper()
	d, err := s.Store.Get(s.Owner, name)
	if err != nil {
		t.Fatalf("Get %q: %v", name, err)
	}
	return *d
}

func saveAndGet(t *testing.T, s Subject) {
	save(t, s, "u1", "ops", "title: Ops\n")
	d := get(t, s, "ops")
	if d.Name != "ops" || d.YAMLBody != "title: Ops\n" || d.CreatedBy != "u1" {
		t.Errorf("dashboard = %+v", d)
	}
	if d.CreatedAt.IsZero() || d.UpdatedAt.IsZero() {
		t.Errorf("times unset: %+v", d)
	}
}

func getMissing(t *testing.T, s Subject) {
	if _, err := s.Store.Get(s.Owner, "nope"); !errors.Is(err, dashboard.ErrNotFound) {
		t.Errorf("Get missing = %v; want ErrNotFound", err)
	}
}

func unchangedSave(t *testing.T, s Subject) {
	save(t, s, "u1", "ops", "title: Ops\n")
	before := get(t, s, "ops")
	time.Sleep(2 * time.Millisecond)
	save(t, s, "u2", "ops", "title: Ops\n")
	if after := get(t, s, "ops"); !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("an unchanged save moved UpdatedAt: %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}
}

func editKeepsCreator(t *testing.T, s Subject) {
	save(t, s, "u1", "ops", "title: Ops\n")
	before := get(t, s, "ops")
	time.Sleep(2 * time.Millisecond)
	save(t, s, "u2", "ops", "title: Operations\n")
	after := get(t, s, "ops")
	if after.YAMLBody != "title: Operations\n" || after.CreatedBy != "u1" {
		t.Errorf("after edit = %+v; want the new body and creator u1", after)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) || !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("times after edit: created %v -> %v, updated %v -> %v", before.CreatedAt, after.CreatedAt, before.UpdatedAt, after.UpdatedAt)
	}
}

func list(t *testing.T, s Subject) {
	if got, err := s.Store.List(s.Owner); err != nil || len(got) != 0 {
		t.Fatalf("List of nothing = %v, %v", got, err)
	}
	save(t, s, "u1", "b", "title: B\n")
	save(t, s, "u1", "a", "title: A\n")
	got, err := s.Store.List(s.Owner)
	if err != nil || len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Errorf("List = %+v, %v; want a then b", got, err)
	}
	if got[0].YAMLBody != "title: A\n" {
		t.Errorf("List carries no body: %+v", got[0])
	}
}

func deleteThenSave(t *testing.T, s Subject) {
	save(t, s, "u1", "ops", "title: Ops\n")
	if err := s.Store.Delete(s.Owner, "ops"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Get(s.Owner, "ops"); !errors.Is(err, dashboard.ErrNotFound) {
		t.Fatalf("Get after Delete = %v; want ErrNotFound", err)
	}
	if got, _ := s.Store.List(s.Owner); len(got) != 0 {
		t.Errorf("List after Delete = %+v", got)
	}
	save(t, s, "u2", "ops", "title: Again\n")
	if d := get(t, s, "ops"); d.CreatedBy != "u2" || d.YAMLBody != "title: Again\n" {
		t.Errorf("recreated = %+v; want creator u2", d)
	}
}

func deleteMissing(t *testing.T, s Subject) {
	if err := s.Store.Delete(s.Owner, "nope"); err != nil {
		t.Errorf("Delete missing = %v", err)
	}
}

func owners(t *testing.T, s Subject) {
	if s.Other == nil {
		t.Skip("store has one owner")
	}
	save(t, s, "u1", "ops", "title: Mine\n")
	if _, err := s.Store.Get(*s.Other, "ops"); !errors.Is(err, dashboard.ErrNotFound) {
		t.Errorf("another owner's Get = %v; want ErrNotFound", err)
	}
	if got, _ := s.Store.List(*s.Other); len(got) != 0 {
		t.Errorf("another owner's List = %+v", got)
	}
	if err := s.Store.Save(*s.Other, "u9", "ops", "title: Theirs\n"); err != nil {
		t.Fatal(err)
	}
	if d := get(t, s, "ops"); d.YAMLBody != "title: Mine\n" {
		t.Errorf("another owner's save changed mine: %+v", d)
	}
}
