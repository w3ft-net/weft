// Package noticestoretest is the conformance suite for notice.Store: the
// behaviour every implementation of the user-notice domain (Postgres for the
// hosted build, SQLite for the open one) must show.
package noticestoretest

import (
	"errors"
	"testing"

	"github.com/w3ft-net/weft/backends/notice"
)

// Subject is one store under test, fresh and empty, and two user ids it accepts.
type Subject struct {
	Store       notice.Store
	User, Other string
}

// Run runs the suite, building a fresh Subject for each test.
func Run(t *testing.T, newSubject func(t *testing.T) Subject) {
	cases := []struct {
		name string
		fn   func(*testing.T, Subject)
	}{
		{"NothingPendingIsAnEmptyListNotAnError", empty},
		{"EmitAndReadBackOldestFirst", emitAndRead},
		{"ParamsRoundTrip", params},
		{"InvalidKindIsRefused", invalidKind},
		{"MarkSeenIsByInstance", markSeen},
		{"MarkSeenOfNothingOrTwiceIsFine", markSeenEdges},
		{"UsersAreSeparate", users},
		{"TheSameKeyTwiceIsTwoInstances", sameKey},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newSubject(t)) })
	}
}

func pending(t *testing.T, s Subject, user string) []notice.Pending {
	t.Helper()
	got, err := s.Store.Pending(user)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if got == nil {
		t.Fatal("Pending returned nil, want a (possibly empty) slice")
	}
	return got
}

func emit(t *testing.T, s Subject, user, key, kind string, params map[string]any) {
	t.Helper()
	if err := s.Store.Emit(user, key, kind, params); err != nil {
		t.Fatalf("Emit %s: %v", key, err)
	}
}

func empty(t *testing.T, s Subject) {
	if got := pending(t, s, s.User); len(got) != 0 {
		t.Fatalf("Pending = %v", got)
	}
}

func emitAndRead(t *testing.T, s Subject) {
	emit(t, s, s.User, "first", notice.KindWarning, nil)
	emit(t, s, s.User, "second", notice.KindSuccess, nil)
	got := pending(t, s, s.User)
	if len(got) != 2 || got[0].Key != "first" || got[1].Key != "second" {
		t.Fatalf("Pending = %+v, want first then second", got)
	}
	if got[0].Kind != notice.KindWarning || got[0].ID == 0 || got[0].CreatedAt.IsZero() {
		t.Fatalf("notice = %+v", got[0])
	}
	if got[0].Params != nil {
		t.Fatalf("Params = %v, want none", got[0].Params)
	}
}

func params(t *testing.T, s Subject) {
	emit(t, s, s.User, "k", notice.KindError, map[string]any{"org": "acme", "groups": []string{"a-w", "b-r"}, "n": 3})
	p := pending(t, s, s.User)[0].Params
	groups, _ := p["groups"].([]any)
	if p["org"] != "acme" || len(groups) != 2 || groups[0] != "a-w" || p["n"] != float64(3) {
		t.Fatalf("Params = %v", p)
	}
}

func invalidKind(t *testing.T, s Subject) {
	if err := s.Store.Emit(s.User, "k", "loud", nil); !errors.Is(err, notice.ErrInvalidKind) {
		t.Fatalf("Emit of an unknown kind = %v, want ErrInvalidKind", err)
	}
	if got := pending(t, s, s.User); len(got) != 0 {
		t.Fatalf("a refused notice was queued: %v", got)
	}
}

func markSeen(t *testing.T, s Subject) {
	emit(t, s, s.User, "a", notice.KindWarning, nil)
	emit(t, s, s.User, "b", notice.KindWarning, nil)
	got := pending(t, s, s.User)
	if err := s.Store.MarkSeen([]int64{got[0].ID}); err != nil {
		t.Fatal(err)
	}
	left := pending(t, s, s.User)
	if len(left) != 1 || left[0].Key != "b" {
		t.Fatalf("Pending after marking a = %+v, want only b", left)
	}
}

func markSeenEdges(t *testing.T, s Subject) {
	if err := s.Store.MarkSeen(nil); err != nil {
		t.Fatalf("MarkSeen(nil): %v", err)
	}
	emit(t, s, s.User, "a", notice.KindWarning, nil)
	id := pending(t, s, s.User)[0].ID
	for i := 0; i < 2; i++ {
		if err := s.Store.MarkSeen([]int64{id}); err != nil {
			t.Fatalf("MarkSeen #%d: %v", i+1, err)
		}
	}
}

func users(t *testing.T, s Subject) {
	emit(t, s, s.User, "mine", notice.KindWarning, nil)
	if got := pending(t, s, s.Other); len(got) != 0 {
		t.Fatalf("another user sees %v", got)
	}
}

func sameKey(t *testing.T, s Subject) {
	emit(t, s, s.User, "k", notice.KindWarning, nil)
	emit(t, s, s.User, "k", notice.KindWarning, nil)
	got := pending(t, s, s.User)
	if len(got) != 2 {
		t.Fatalf("Pending = %d, want two instances", len(got))
	}
	if err := s.Store.MarkSeen([]int64{got[0].ID}); err != nil {
		t.Fatal(err)
	}
	if left := pending(t, s, s.User); len(left) != 1 || left[0].ID != got[1].ID {
		t.Fatalf("marking one instance burned the other: %+v", left)
	}
}
