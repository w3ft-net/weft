package store

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests for the token auth cache (docs/plans/weftd-offset-checkpointing.md,
// "Stream-open cost"). Ack cycling makes IngestStream opens steady-state, so
// ValidateAgentToken must not hit the database on a warm token.

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// noJitter makes TTLs exact so expiry assertions are deterministic.
func noJitter(base time.Duration) time.Duration { return base }

func testAuthCache(clk *fakeClock) *authCache {
	c := newAuthCache(60*time.Second, 5*time.Second, 100)
	c.now = clk.Now
	c.jitter = noJitter
	return c
}

// countAuthStatements counts SQL statements touching the auth tables
// (agent_tokens plus the two Preload targets) issued through db.
func countAuthStatements(t *testing.T, db *Store) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	remove := db.OnSQLForTest("count_auth_"+t.Name(), func(sql string) {
		for _, tbl := range []string{`"agent_tokens"`, `"organizations"`, `"namespaces"`} {
			if strings.Contains(sql, tbl) {
				n.Add(1)
				return
			}
		}
	})
	t.Cleanup(remove)
	return &n
}

func TestAuthCache_WarmTokenHitsNoDB(t *testing.T) {
	db := testOpen(t)
	clk := &fakeClock{t: time.Now()}
	db.authCache = testAuthCache(clk)

	cust, err := db.FindOrCreateOrg("authcache-hit")
	if err != nil {
		t.Fatal(err)
	}
	raw, tok, err := createTestToken(db, cust.ID, "hit")
	if err != nil {
		t.Fatal(err)
	}

	stmts := countAuthStatements(t, db)
	first, err := db.ValidateAgentToken(raw)
	if err != nil || first == nil {
		t.Fatalf("first validate: %v %v", first, err)
	}
	cold := stmts.Load()
	if cold == 0 {
		t.Fatal("cold lookup issued no statements; counter is not wired")
	}
	for i := 0; i < 20; i++ {
		got, err := db.ValidateAgentToken(raw)
		if err != nil || got == nil || got.TokenID != tok.ID {
			t.Fatalf("warm validate %d: %+v %v", i, got, err)
		}
	}
	if extra := stmts.Load() - cold; extra != 0 {
		t.Errorf("warm validates issued %d auth statements, want 0", extra)
	}
}

func TestAuthCache_ExpiresAfterTTL(t *testing.T) {
	db := testOpen(t)
	clk := &fakeClock{t: time.Now()}
	db.authCache = testAuthCache(clk)

	cust, _ := db.FindOrCreateOrg("authcache-expire")
	raw, _, _ := createTestToken(db, cust.ID, "expire")

	stmts := countAuthStatements(t, db)
	db.ValidateAgentToken(raw)
	cold := stmts.Load()

	clk.Advance(59 * time.Second)
	db.ValidateAgentToken(raw)
	if stmts.Load() != cold {
		t.Error("lookup before TTL should be served from cache")
	}
	clk.Advance(2 * time.Second)
	db.ValidateAgentToken(raw)
	if stmts.Load() == cold {
		t.Error("lookup after TTL should hit the DB")
	}
}

func TestAuthCache_NegativeEntryIsShortLived(t *testing.T) {
	db := testOpen(t)
	clk := &fakeClock{t: time.Now()}
	db.authCache = testAuthCache(clk)

	stmts := countAuthStatements(t, db)
	for i := 0; i < 5; i++ {
		got, err := db.ValidateAgentToken("wft_doesnotexist")
		if err != nil || got != nil {
			t.Fatalf("bogus token: %+v %v", got, err)
		}
	}
	if n := stmts.Load(); n != 1 {
		t.Errorf("5 bogus validates issued %d statements, want 1 (negative cached)", n)
	}

	// A token minted right after being probed must validate once the
	// short negative TTL passes.
	cust, _ := db.FindOrCreateOrg("authcache-neg")
	ns, _ := db.FindOrCreateNamespace(cust.ID, "test")
	raw, _, err := db.CreateAgentToken(cust.ID, ns.ID, "neg", "")
	if err != nil {
		t.Fatal(err)
	}
	db.ValidateAgentToken(raw) // cold, caches positive
	clk.Advance(6 * time.Second)
	if got, _ := db.ValidateAgentToken("wft_doesnotexist"); got != nil {
		t.Error("bogus token validated")
	}
	if n := stmts.Load(); n < 2 {
		t.Errorf("negative entry should have expired after 5s; statements=%d", n)
	}
}

func TestAuthCache_RevokeInvalidatesImmediately(t *testing.T) {
	db := testOpen(t)
	clk := &fakeClock{t: time.Now()}
	db.authCache = testAuthCache(clk)

	cust, _ := db.FindOrCreateOrg("authcache-revoke")
	raw, tok, _ := createTestToken(db, cust.ID, "revoke")

	if got, _ := db.ValidateAgentToken(raw); got == nil {
		t.Fatal("token should validate before revoke")
	}
	if err := db.RevokeAgentToken(tok.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := db.ValidateAgentToken(raw); err != nil || got != nil {
		t.Errorf("revoked token still validates from cache: %+v %v", got, err)
	}
}

func TestAuthCache_ReturnsCopies(t *testing.T) {
	db := testOpen(t)
	clk := &fakeClock{t: time.Now()}
	db.authCache = testAuthCache(clk)

	cust, _ := db.FindOrCreateOrg("authcache-copy")
	raw, _, _ := createTestToken(db, cust.ID, "copy")

	a, _ := db.ValidateAgentToken(raw)
	a.Organization.Name = "mutated"
	a.Namespace = "mutated"
	b, _ := db.ValidateAgentToken(raw)
	if b.Organization.Name == "mutated" || b.Namespace == "mutated" {
		t.Error("cache handed out shared state; callers can corrupt it")
	}
}

func TestAuthCache_SingleflightCoalescesConcurrentMisses(t *testing.T) {
	c := testAuthCache(&fakeClock{t: time.Now()})

	var loads atomic.Int64
	release := make(chan struct{})
	load := func() (*AgentAuthResult, error) {
		loads.Add(1)
		<-release
		return &AgentAuthResult{TokenID: 7, Organization: &Organization{}}, nil
	}

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := c.get("hash", load); err != nil || got == nil || got.TokenID != 7 {
				t.Errorf("get: %+v %v", got, err)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond) // let the herd pile up behind the loader
	close(release)
	wg.Wait()

	if got := loads.Load(); got != 1 {
		t.Errorf("%d concurrent misses ran the loader %d times, want 1", n, got)
	}
}

func TestAuthCache_JitterSpreadsExpiry(t *testing.T) {
	base := 60 * time.Second
	seen := map[time.Duration]bool{}
	for i := 0; i < 500; i++ {
		d := defaultJitter(base)
		if d < 54*time.Second || d > 66*time.Second {
			t.Fatalf("jittered ttl %v outside [54s, 66s]", d)
		}
		seen[d] = true
	}
	if len(seen) < 100 {
		t.Errorf("only %d distinct TTLs in 500 draws; jitter is not spreading", len(seen))
	}
}

func TestAuthCache_SizeIsBounded(t *testing.T) {
	clk := &fakeClock{t: time.Now()}
	c := newAuthCache(60*time.Second, 5*time.Second, 50)
	c.now, c.jitter = clk.Now, noJitter

	for i := 0; i < 500; i++ {
		c.get(fmt.Sprintf("bogus-%d", i), func() (*AgentAuthResult, error) { return nil, nil })
	}
	if n := c.len(); n > 50 {
		t.Errorf("cache grew to %d entries, cap is 50", n)
	}
}
