package weftd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/kevlarcon/weft/ingest"
)

// ParserCache holds per-app parsers built from schema YAML returned by
// the coordinator. Parsers are refreshed when the schema_hash changes;
// the tailer reads the parser pointer via atomic.Load on each line so
// a schema swap during live ingest is non-blocking.
type ParserCache struct {
	mu   sync.RWMutex
	apps map[string]*ParserSlot
}

// ParserSlot is the per-app holder. Multiple goroutines may read
// Parser() concurrently; only the refresh loop calls Install().
type ParserSlot struct {
	parser atomic.Pointer[ingest.Parser]
	hash   atomic.Pointer[string]

	// hasLevelField is true when the schema declares a level_field that
	// the parser will try to extract. If so, an empty Level on a parsed
	// row is treated as a parse miss for stats reporting. Atomic
	// because Install (refresh loop) writes it concurrently with the
	// row-stamping path's reads.
	hasLevelField atomic.Bool

	// Parse statistics, incremented in the hot path, drained by the
	// stats reporter.
	okCount      atomic.Int64
	missingLevel atomic.Int64
}

func NewParserCache() *ParserCache {
	return &ParserCache{apps: make(map[string]*ParserSlot)}
}

// Slot returns the ParserSlot for app, creating it on first call.
// The slot exists even when no parser has been installed yet; callers
// must check Parser() for nil before using it.
func (c *ParserCache) Slot(app string) *ParserSlot {
	c.mu.RLock()
	slot, ok := c.apps[app]
	c.mu.RUnlock()
	if ok {
		return slot
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	slot, ok = c.apps[app]
	if !ok {
		slot = &ParserSlot{}
		c.apps[app] = slot
	}
	return slot
}

// Apps returns a snapshot of the app names currently in the cache.
// Used by the refresh loop to know what to poll.
func (c *ParserCache) Apps() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.apps))
	for k := range c.apps {
		out = append(out, k)
	}
	return out
}

// Install parses schemaYAML, builds a parser, and atomically swaps it
// into the slot for app. The schema's app/customer fields are forced
// to the agent-supplied values, matching what the CloudWatch poller
// does (coordinator/poller/poller.go:90-91).
//
// Returns the computed hash so the caller can cache it. If the YAML
// fails to parse, returns the error and leaves the slot unchanged.
func (c *ParserCache) Install(app, customer, schemaYAML string) (string, error) {
	schema, err := ingest.LoadSchemaFromBytes([]byte(schemaYAML))
	if err != nil {
		return "", fmt.Errorf("load schema: %w", err)
	}
	schema.App = app
	schema.Customer = customer

	parser, err := ingest.NewParser(schema)
	if err != nil {
		return "", fmt.Errorf("new parser: %w", err)
	}

	hash := HashSchema(schemaYAML)
	slot := c.Slot(app)
	slot.parser.Store(parser)
	slot.hash.Store(&hash)
	slot.hasLevelField.Store(schema.LevelField != "" && schema.LevelField != "none")
	return hash, nil
}

// HashSchema returns the SHA-256 hex digest of the YAML bytes. Same
// representation the coordinator returns in GetSourceStatusResponse.SchemaHash.
func HashSchema(schemaYAML string) string {
	sum := sha256.Sum256([]byte(schemaYAML))
	return hex.EncodeToString(sum[:])
}

// Parser returns the currently installed parser, or nil if none.
func (s *ParserSlot) Parser() *ingest.Parser {
	return s.parser.Load()
}

// Hash returns the schema hash of the currently installed parser.
func (s *ParserSlot) Hash() string {
	if h := s.hash.Load(); h != nil {
		return *h
	}
	return ""
}

// HasLevelField reports whether the schema declares level extraction.
// Used by the row-stamping path to decide whether an empty Level is a
// parse miss worth counting.
func (s *ParserSlot) HasLevelField() bool {
	return s.hasLevelField.Load()
}

// RecordOK increments the per-app successful parse counter.
func (s *ParserSlot) RecordOK() { s.okCount.Add(1) }

// RecordMissingLevel increments the per-app missing-level counter.
// The line is still shipped; the counter exists so a misconfigured
// schema is greppable from the agent host.
func (s *ParserSlot) RecordMissingLevel() { s.missingLevel.Add(1) }

// SwapStats atomically reads-and-zeros the counters. Returns the
// counts since the last call. Used by the periodic stats reporter.
func (s *ParserSlot) SwapStats() (ok, missingLevel int64) {
	ok = s.okCount.Swap(0)
	missingLevel = s.missingLevel.Swap(0)
	return
}
