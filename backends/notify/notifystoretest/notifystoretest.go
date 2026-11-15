// Package notifystoretest is the conformance suite for notify.Store and
// notify.Secrets: the behaviour every implementation of the
// notification-configuration domain (Postgres for the hosted build, SQLite for
// the open one) must show.
package notifystoretest

import (
	"errors"
	"slices"
	"testing"

	"github.com/w3ft-net/weft/backends/notify"
	"github.com/w3ft-net/weft/shape"
)

// Subject is one pair of stores under test, fresh and empty.
type Subject struct {
	Aliases notify.Store
	Secrets notify.Secrets
	// Owner owns what the suite makes. The open build's is the empty value.
	Owner shape.Added
	// Other is a second owner, for a store that has owners to tell apart. Nil for
	// one that has none.
	Other *shape.Added
	// User is a user id the store accepts as an author.
	User, OtherUser string
}

// Run runs the suite, building a fresh Subject for each test.
func Run(t *testing.T, newSubject func(t *testing.T) Subject) {
	cases := []struct {
		name string
		fn   func(*testing.T, Subject)
	}{
		{"AliasesAreEmptyBeforeTheFirstSave", aliasesEmpty},
		{"SaveAndRead", saveAndRead},
		{"SavingTheSameTextChangesNothing", sameText},
		{"AmendMessage", amend},
		{"AmendMessageNeedsADocument", amendMissing},
		{"AmendMessageByAuthor", amendByAuthor},
		{"AliasesOwnersAreSeparate", aliasOwners},
		{"SecretsSetReadAndList", secretsBasic},
		{"SecretReplace", secretReplace},
		{"SecretDelete", secretDelete},
		{"SecretsOwnersAreSeparate", secretOwners},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newSubject(t)) })
	}
}

func aliases(t *testing.T, s Subject, owner shape.Added) string {
	t.Helper()
	got, err := s.Aliases.Aliases(owner)
	if err != nil {
		t.Fatalf("Aliases: %v", err)
	}
	if (got.Text == "") != got.UpdatedAt.IsZero() {
		t.Fatalf("Aliases = %+v: a saved document has a time, and an unsaved one has none", got)
	}
	return got.Text
}

func aliasesEmpty(t *testing.T, s Subject) {
	if got := aliases(t, s, s.Owner); got != "" {
		t.Fatalf("Aliases = %q, want empty", got)
	}
}

func saveAndRead(t *testing.T, s Subject) {
	if err := s.Aliases.SaveAliases(s.Owner, s.User, "system:\n  email: a@x.test\n"); err != nil {
		t.Fatal(err)
	}
	if got := aliases(t, s, s.Owner); got != "system:\n  email: a@x.test\n" {
		t.Fatalf("Aliases = %q", got)
	}
	if err := s.Aliases.SaveAliases(s.Owner, s.User, "system:\n  email: b@x.test\n"); err != nil {
		t.Fatal(err)
	}
	if got := aliases(t, s, s.Owner); got != "system:\n  email: b@x.test\n" {
		t.Fatalf("Aliases after an edit = %q", got)
	}
}

func sameText(t *testing.T, s Subject) {
	for i := 0; i < 2; i++ {
		if err := s.Aliases.SaveAliases(s.Owner, s.User, "system:\n  email: a@x.test\n"); err != nil {
			t.Fatal(err)
		}
	}
	if got := aliases(t, s, s.Owner); got != "system:\n  email: a@x.test\n" {
		t.Fatalf("Aliases = %q", got)
	}
}

func amend(t *testing.T, s Subject) {
	if err := s.Aliases.SaveAliases(s.Owner, s.User, "a: 1\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.Aliases.AmendMessage(s.Owner, "", "Route the pager"); err != nil {
		t.Fatalf("AmendMessage: %v", err)
	}
}

func amendMissing(t *testing.T, s Subject) {
	if err := s.Aliases.AmendMessage(s.Owner, "", "x"); !errors.Is(err, notify.ErrNotFound) {
		t.Fatalf("AmendMessage with no document = %v, want ErrNotFound", err)
	}
}

func amendByAuthor(t *testing.T, s Subject) {
	if s.User == "" || s.OtherUser == "" {
		t.Skip("no authors to tell apart")
	}
	if err := s.Aliases.SaveAliases(s.Owner, s.User, "a: 1\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.Aliases.AmendMessage(s.Owner, s.OtherUser, "mine"); !errors.Is(err, notify.ErrNotYours) {
		t.Fatalf("amend by another user = %v, want ErrNotYours", err)
	}
	if err := s.Aliases.AmendMessage(s.Owner, s.User, "mine"); err != nil {
		t.Fatalf("amend by the author: %v", err)
	}
}

func aliasOwners(t *testing.T, s Subject) {
	if s.Other == nil {
		t.Skip("one implicit owner")
	}
	if err := s.Aliases.SaveAliases(s.Owner, s.User, "mine: 1\n"); err != nil {
		t.Fatal(err)
	}
	if got := aliases(t, s, *s.Other); got != "" {
		t.Fatalf("the other owner sees %q", got)
	}
}

func names(t *testing.T, s Subject, owner shape.Added) []string {
	t.Helper()
	got, err := s.Secrets.Names(owner)
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	if got == nil {
		t.Fatal("Names returned nil, want a (possibly empty) slice")
	}
	return got
}

func secretsBasic(t *testing.T, s Subject) {
	if got := names(t, s, s.Owner); len(got) != 0 {
		t.Fatalf("Names = %v, want none", got)
	}
	for _, n := range []string{"zeta", "alpha"} {
		if err := s.Secrets.Set(s.Owner, n, "v-"+n); err != nil {
			t.Fatal(err)
		}
	}
	if got := names(t, s, s.Owner); !slices.Equal(got, []string{"alpha", "zeta"}) {
		t.Fatalf("Names = %v", got)
	}
	if v, err := s.Secrets.Value(s.Owner, "alpha"); err != nil || v != "v-alpha" {
		t.Fatalf("Value = %q %v", v, err)
	}
	if _, err := s.Secrets.Value(s.Owner, "missing"); !errors.Is(err, notify.ErrNotFound) {
		t.Fatalf("Value of a missing secret = %v", err)
	}
}

func secretReplace(t *testing.T, s Subject) {
	for _, v := range []string{"one", "two"} {
		if err := s.Secrets.Set(s.Owner, "k", v); err != nil {
			t.Fatal(err)
		}
	}
	if v, _ := s.Secrets.Value(s.Owner, "k"); v != "two" {
		t.Fatalf("Value = %q, want the replacement", v)
	}
	if got := names(t, s, s.Owner); len(got) != 1 {
		t.Fatalf("Names = %v, want one", got)
	}
}

func secretDelete(t *testing.T, s Subject) {
	if err := s.Secrets.Set(s.Owner, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := s.Secrets.Delete(s.Owner, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Secrets.Value(s.Owner, "k"); !errors.Is(err, notify.ErrNotFound) {
		t.Fatalf("deleted secret still there: %v", err)
	}
	if err := s.Secrets.Delete(s.Owner, "k"); !errors.Is(err, notify.ErrNotFound) {
		t.Fatalf("deleting a missing secret = %v, want ErrNotFound", err)
	}
	// Set again: a delete is outright, so the name is free.
	if err := s.Secrets.Set(s.Owner, "k", "again"); err != nil {
		t.Fatal(err)
	}
}

func secretOwners(t *testing.T, s Subject) {
	if s.Other == nil {
		t.Skip("one implicit owner")
	}
	if err := s.Secrets.Set(s.Owner, "k", "mine"); err != nil {
		t.Fatal(err)
	}
	if got := names(t, s, *s.Other); len(got) != 0 {
		t.Fatalf("the other owner sees %v", got)
	}
	if _, err := s.Secrets.Value(*s.Other, "k"); !errors.Is(err, notify.ErrNotFound) {
		t.Fatalf("the other owner read the secret: %v", err)
	}
	if err := s.Secrets.Set(*s.Other, "k", "theirs"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Secrets.Value(s.Owner, "k"); v != "mine" {
		t.Fatalf("the other owner's write changed mine: %q", v)
	}
}
