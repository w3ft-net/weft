// Package loginstoretest is the conformance suite for login.Store: the behaviour
// every implementation of the sign-in domain must show. An implementation's test
// calls Run with a constructor.
package loginstoretest

import (
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/login"
)

// Subject is one store under test, fresh and empty.
type Subject struct {
	Store login.Store
	// Allow puts email on the list of addresses that may sign in. The contract
	// has no way to do it, because nothing but an operator does, so the
	// implementation's own test says how.
	Allow func(t *testing.T, email string)
}

// Run runs the suite, building a fresh Subject for each test.
func Run(t *testing.T, newSubject func(t *testing.T) Subject) {
	cases := []struct {
		name string
		fn   func(*testing.T, Subject)
	}{
		{"Allowed", allowed},
		{"IssueLinkMakesARawToken", issueForm},
		{"ConsumeLinkReturnsTheAddress", consumeReturnsAddress},
		{"ALinkWorksOnce", linkWorksOnce},
		{"AnExpiredLinkIsRefused", expiredLink},
		{"AnUnknownLinkIsRefused", unknownLink},
		{"ConcurrentConsumesSucceedOnce", concurrentConsume},
		{"LinksAreIndependent", independentLinks},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newSubject(t)) })
	}
}

var rawForm = regexp.MustCompile(`^lnk_[0-9a-f]{64}$`)

func issue(t *testing.T, s Subject, email string, ttl time.Duration) string {
	t.Helper()
	raw, err := s.Store.IssueLink(email, ttl)
	if err != nil {
		t.Fatalf("IssueLink: %v", err)
	}
	return raw
}

func allowed(t *testing.T, s Subject) {
	ok, err := s.Store.Allowed("ada@example.test")
	if err != nil || ok {
		t.Fatalf("an address nobody added is allowed: ok=%v err=%v", ok, err)
	}
	s.Allow(t, "ada@example.test")
	for _, addr := range []string{"ada@example.test", "ADA@Example.Test", "  ada@example.test "} {
		if ok, err := s.Store.Allowed(addr); err != nil || !ok {
			t.Errorf("Allowed(%q) = %v, %v; want true", addr, ok, err)
		}
	}
	if ok, _ := s.Store.Allowed("grace@example.test"); ok {
		t.Error("another address is allowed because one was added")
	}
}

func issueForm(t *testing.T, s Subject) {
	raw := issue(t, s, "ada@example.test", time.Hour)
	if !rawForm.MatchString(raw) {
		t.Errorf("raw token %q is not lnk_ and 64 hex digits", raw)
	}
	if other := issue(t, s, "ada@example.test", time.Hour); other == raw {
		t.Error("two issues made the same token")
	}
}

func consumeReturnsAddress(t *testing.T, s Subject) {
	raw := issue(t, s, "Ada@Example.Test", time.Hour)
	email, ok, err := s.Store.ConsumeLink(raw)
	if err != nil || !ok {
		t.Fatalf("ConsumeLink = %q, %v, %v; want ok", email, ok, err)
	}
	if email != "ada@example.test" {
		t.Errorf("address = %q; want the normalized form", email)
	}
}

func linkWorksOnce(t *testing.T, s Subject) {
	raw := issue(t, s, "ada@example.test", time.Hour)
	if _, ok, _ := s.Store.ConsumeLink(raw); !ok {
		t.Fatal("first consume refused")
	}
	if email, ok, err := s.Store.ConsumeLink(raw); ok || err != nil {
		t.Errorf("second consume = %q, %v, %v; want a refusal without an error", email, ok, err)
	}
}

func expiredLink(t *testing.T, s Subject) {
	raw := issue(t, s, "ada@example.test", -time.Second)
	if email, ok, err := s.Store.ConsumeLink(raw); ok || err != nil {
		t.Errorf("expired consume = %q, %v, %v; want a refusal without an error", email, ok, err)
	}
}

func unknownLink(t *testing.T, s Subject) {
	for _, raw := range []string{"", "lnk_" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "garbage"} {
		if _, ok, err := s.Store.ConsumeLink(raw); ok || err != nil {
			t.Errorf("ConsumeLink(%q) = %v, %v; want a refusal without an error", raw, ok, err)
		}
	}
}

func concurrentConsume(t *testing.T, s Subject) {
	raw := issue(t, s, "ada@example.test", time.Hour)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok, _ := s.Store.ConsumeLink(raw); ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := wins.Load(); n != 1 {
		t.Errorf("%d concurrent consumes succeeded; want exactly 1", n)
	}
}

func independentLinks(t *testing.T, s Subject) {
	a := issue(t, s, "ada@example.test", time.Hour)
	g := issue(t, s, "grace@example.test", time.Hour)
	if email, ok, _ := s.Store.ConsumeLink(g); !ok || email != "grace@example.test" {
		t.Fatalf("grace's link = %q, %v", email, ok)
	}
	if email, ok, _ := s.Store.ConsumeLink(a); !ok || email != "ada@example.test" {
		t.Errorf("ada's link after grace's was spent = %q, %v", email, ok)
	}
}
