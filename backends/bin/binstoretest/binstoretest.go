// Package binstoretest is the conformance suite for bin.Store: the behaviour
// every implementation of the /bin domain (Postgres for the hosted build, SQLite
// for the open one) must show. An implementation's test calls Run with a
// constructor.
package binstoretest

import (
	"errors"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/shape"
)

// Subject is one store under test, fresh and empty.
type Subject struct {
	Store bin.Store
	// Owner owns the scripts the suite makes. The open build's is the empty
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
		{"EditKeepsTheCreatorAndSetsTheAuthor", editAttribution},
		{"ListIsByName", list},
		{"DeleteAndRevive", deleteAndRevive},
		{"AmendMessage", amendMessage},
		{"AmendMessageByAuthor", amendByAuthor},
		{"LanguageIsAProperty", language},
		{"OwnersAreSeparate", owners},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newSubject(t)) })
	}
}

func save(t *testing.T, s Subject, user, name, source string) {
	t.Helper()
	if err := s.Store.Save(s.Owner, user, name, source, bin.LangBash); err != nil {
		t.Fatalf("Save %q: %v", name, err)
	}
}

func get(t *testing.T, s Subject, name string) bin.Script {
	t.Helper()
	sc, err := s.Store.Get(s.Owner, name)
	if err != nil {
		t.Fatalf("Get %q: %v", name, err)
	}
	return *sc
}

func saveAndGet(t *testing.T, s Subject) {
	save(t, s, "u1", "scan", "echo hi\n")
	sc := get(t, s, "scan")
	if sc.Name != "scan" || sc.Source != "echo hi\n" || sc.Language != bin.LangBash {
		t.Errorf("script = %+v", sc)
	}
	if sc.ContentHash != bin.ContentHash("echo hi\n") {
		t.Errorf("ContentHash = %q, want the hash of the source", sc.ContentHash)
	}
	if sc.CreatedBy != "u1" || sc.AuthoredBy != "u1" {
		t.Errorf("created by %q, authored by %q, want u1 for both", sc.CreatedBy, sc.AuthoredBy)
	}
	if sc.Message != "" || sc.ID == 0 || sc.CreatedAt.IsZero() || sc.UpdatedAt.IsZero() {
		t.Errorf("script = %+v", sc)
	}
}

func getMissing(t *testing.T, s Subject) {
	if _, err := s.Store.Get(s.Owner, "nope"); !errors.Is(err, bin.ErrNotFound) {
		t.Fatalf("Get of a missing script: %v, want ErrNotFound", err)
	}
	if err := s.Store.AmendMessage(s.Owner, "", "nope", "m"); !errors.Is(err, bin.ErrNotFound) {
		t.Fatalf("AmendMessage of a missing script: %v, want ErrNotFound", err)
	}
}

// Writing back the source a script already has leaves its version alone: the
// time, the author and the message it carries.
func unchangedSave(t *testing.T, s Subject) {
	save(t, s, "u1", "scan", "echo hi\n")
	if err := s.Store.AmendMessage(s.Owner, "", "scan", "first"); err != nil {
		t.Fatal(err)
	}
	before := get(t, s, "scan")
	time.Sleep(5 * time.Millisecond)
	save(t, s, "u2", "scan", "echo hi\n")
	after := get(t, s, "scan")
	if !after.UpdatedAt.Equal(before.UpdatedAt) || after.AuthoredBy != "u1" || after.Message != "first" {
		t.Fatalf("an unchanged save changed the script: before %+v, after %+v", before, after)
	}
}

func editAttribution(t *testing.T, s Subject) {
	save(t, s, "u1", "scan", "echo one\n")
	if err := s.Store.AmendMessage(s.Owner, "", "scan", "first"); err != nil {
		t.Fatal(err)
	}
	save(t, s, "u2", "scan", "echo two\n")
	sc := get(t, s, "scan")
	if sc.Source != "echo two\n" || sc.ContentHash != bin.ContentHash("echo two\n") {
		t.Errorf("the edit was not kept: %+v", sc)
	}
	if sc.CreatedBy != "u1" || sc.AuthoredBy != "u2" {
		t.Errorf("created by %q, authored by %q, want u1 and u2: an edit is not authoring", sc.CreatedBy, sc.AuthoredBy)
	}
	if sc.Message != "" {
		t.Errorf("message = %q, want it cleared by the new version", sc.Message)
	}
}

func list(t *testing.T, s Subject) {
	for _, n := range []string{"b", "c", "a"} {
		save(t, s, "u1", n, "echo "+n+"\n")
	}
	got, err := s.Store.List(s.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "a" || got[1].Name != "b" || got[2].Name != "c" {
		t.Fatalf("List = %+v, want a, b, c", got)
	}
}

func deleteAndRevive(t *testing.T, s Subject) {
	save(t, s, "u1", "scan", "echo hi\n")
	if err := s.Store.Delete(s.Owner, "scan"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Get(s.Owner, "scan"); !errors.Is(err, bin.ErrNotFound) {
		t.Fatalf("Get after Delete: %v, want ErrNotFound", err)
	}
	if got, _ := s.Store.List(s.Owner); len(got) != 0 {
		t.Fatalf("List after Delete = %+v", got)
	}
	if err := s.Store.Delete(s.Owner, "scan"); err != nil {
		t.Fatalf("Delete of a deleted script: %v", err)
	}
	if err := s.Store.Delete(s.Owner, "never"); err != nil {
		t.Fatalf("Delete of a missing script: %v", err)
	}

	// Saving over the deleted name brings it back, even with the same source, and
	// it keeps the creator it had.
	save(t, s, "u2", "scan", "echo hi\n")
	sc := get(t, s, "scan")
	if sc.CreatedBy != "u1" || sc.AuthoredBy != "u2" || sc.Source != "echo hi\n" {
		t.Errorf("revived script = %+v, want created by u1, authored by u2", sc)
	}
}

func amendMessage(t *testing.T, s Subject) {
	save(t, s, "u1", "scan", "echo hi\n")
	if err := s.Store.AmendMessage(s.Owner, "", "scan", "Count the errors"); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, "scan").Message; got != "Count the errors" {
		t.Fatalf("Message = %q", got)
	}
}

func amendByAuthor(t *testing.T, s Subject) {
	save(t, s, "u1", "scan", "echo one\n")
	save(t, s, "u2", "scan", "echo two\n")
	if err := s.Store.AmendMessage(s.Owner, "u1", "scan", "mine"); !errors.Is(err, bin.ErrNotYours) {
		t.Fatalf("u1 amending u2's version: %v, want ErrNotYours", err)
	}
	if got := get(t, s, "scan").Message; got != "" {
		t.Fatalf("a refused amendment changed the message to %q", got)
	}
	if err := s.Store.AmendMessage(s.Owner, "u2", "scan", "mine"); err != nil {
		t.Fatalf("the author amending: %v", err)
	}
	if got := get(t, s, "scan").Message; got != "mine" {
		t.Fatalf("Message = %q", got)
	}
}

func language(t *testing.T, s Subject) {
	if err := s.Store.Save(s.Owner, "u1", "sev", "print(1)\n", bin.LangLua); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, "sev").Language; got != bin.LangLua {
		t.Fatalf("Language = %q, want lua", got)
	}
	// The language follows an edit, whatever the name.
	save(t, s, "u1", "sev", "echo 1\n")
	if got := get(t, s, "sev").Language; got != bin.LangBash {
		t.Fatalf("Language after an edit = %q, want bash", got)
	}
}

func owners(t *testing.T, s Subject) {
	if s.Other == nil {
		t.Skip("the store has one owner")
	}
	save(t, s, "u1", "scan", "echo mine\n")
	if err := s.Store.Save(*s.Other, "u9", "scan", "echo theirs\n", bin.LangBash); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, "scan").Source; got != "echo mine\n" {
		t.Errorf("owner's script = %q", got)
	}
	theirs, err := s.Store.Get(*s.Other, "scan")
	if err != nil || theirs.Source != "echo theirs\n" {
		t.Errorf("other's script = %+v, err=%v", theirs, err)
	}
	if err := s.Store.Delete(*s.Other, "scan"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Get(s.Owner, "scan"); err != nil {
		t.Errorf("deleting the other's script took the owner's: %v", err)
	}
	if got, _ := s.Store.List(*s.Other); len(got) != 0 {
		t.Errorf("other's List = %+v", got)
	}
}
