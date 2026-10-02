package weftd

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testSchemaA = `envelope: none
format: text
timestamp_field: "1"
timestamp_fields: 2
timestamp_format: "2006-01-02 15:04:05"
level_field: "3"
`

const testSchemaB = `envelope: none
format: text
timestamp_field: "1"
timestamp_fields: 2
timestamp_format: "2006-01-02 15:04:05"
level_field: "3"
level_pattern: "^([A-Z]+)$"
`

func TestParserCacheInstall(t *testing.T) {
	c := NewParserCache()
	hashA, err := c.Install("ns/api", testSchemaA)
	if err != nil {
		t.Fatalf("install A: %v", err)
	}
	if c.Slot("ns/api").Hash() != hashA {
		t.Fatalf("hash mismatch after install A: want %s got %s", hashA, c.Slot("ns/api").Hash())
	}
	if c.Slot("ns/api").Parser() == nil {
		t.Fatal("parser nil after install A")
	}
	if !c.Slot("ns/api").HasLevelField() {
		t.Fatal("HasLevelField should be true for schema A")
	}
}

func TestParserCacheReinstallChangesHash(t *testing.T) {
	c := NewParserCache()
	hashA, err := c.Install("ns/api", testSchemaA)
	if err != nil {
		t.Fatalf("install A: %v", err)
	}
	hashB, err := c.Install("ns/api", testSchemaB)
	if err != nil {
		t.Fatalf("install B: %v", err)
	}
	if hashA == hashB {
		t.Fatalf("expected different hashes for different YAML; got %s == %s", hashA, hashB)
	}
	if c.Slot("ns/api").Hash() != hashB {
		t.Fatalf("slot hash should be hashB after reinstall")
	}
}

func TestParserCacheReinstallSameYAMLStableHash(t *testing.T) {
	c := NewParserCache()
	hash1, err := c.Install("ns/api", testSchemaA)
	if err != nil {
		t.Fatal(err)
	}
	hash2, err := c.Install("ns/api", testSchemaA)
	if err != nil {
		t.Fatal(err)
	}
	if hash1 != hash2 {
		t.Fatalf("expected stable hash for unchanged YAML; got %s != %s", hash1, hash2)
	}
}

func TestParserCacheStatsSwap(t *testing.T) {
	c := NewParserCache()
	if _, err := c.Install("ns/api", testSchemaA); err != nil {
		t.Fatal(err)
	}
	slot := c.Slot("ns/api")
	slot.RecordOK()
	slot.RecordOK()
	slot.RecordMissingLevel()
	ok, missing := slot.SwapStats()
	if ok != 2 || missing != 1 {
		t.Fatalf("want ok=2 missing=1, got ok=%d missing=%d", ok, missing)
	}
	// Second swap returns zero — the values were drained.
	ok, missing = slot.SwapStats()
	if ok != 0 || missing != 0 {
		t.Fatalf("want zeros after drain, got ok=%d missing=%d", ok, missing)
	}
}

// TestParserCacheConcurrentSwap exercises the atomic-pointer swap that
// keeps live tailers from observing a half-installed parser. Many
// readers call Slot.Parser() concurrently while a writer reinstalls
// schemas in a tight loop. The test passes if no nil pointer is
// observed and the race detector finds nothing.
//
// Note: we deliberately do NOT call ParseLine concurrently. Each
// parser is owned by a single tailer goroutine in production
// (one tailer per app), so Parser.lineNum is not synchronised. The
// swap-atomicity guarantee this test cares about is the pointer
// load — once a reader has hold of a *Parser, it's free to use it
// alone without coordination.
func TestParserCacheConcurrentSwap(t *testing.T) {
	c := NewParserCache()
	if _, err := c.Install("ns/api", testSchemaA); err != nil {
		t.Fatal(err)
	}
	slot := c.Slot("ns/api")

	var stop atomic.Bool
	var wg sync.WaitGroup
	const readers = 8
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for !stop.Load() {
				if slot.Parser() == nil {
					t.Errorf("observed nil parser during swap")
					return
				}
				_ = slot.Hash()
				_ = slot.HasLevelField()
			}
		}()
	}

	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := c.Install("ns/api", testSchemaB); err != nil {
			t.Errorf("install B: %v", err)
			break
		}
		if _, err := c.Install("ns/api", testSchemaA); err != nil {
			t.Errorf("install A: %v", err)
			break
		}
	}
	stop.Store(true)
	wg.Wait()
}

func TestHashSchemaStable(t *testing.T) {
	if HashSchema("foo\n") != HashSchema("foo\n") {
		t.Fatal("HashSchema not deterministic")
	}
	if HashSchema("foo") == HashSchema("bar") {
		t.Fatal("distinct inputs produced identical hash")
	}
}
