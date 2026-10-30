package shard

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/warp/pkg/awk"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// Field facts are what this shard has learned about the values of an awk
// field ("$N") or a top-level payload key (".key") in each source: whether
// every row it has seen held a number there. The planner lowers `$5 > 500`
// or `select(.q_max_ms > 10)` exactly only where that is so. See
// docs/plans/field-types.md.
//
// A fact is a guess promoted by evidence. A locator is Typed once
// factTypedAfter rows of its source have all had a numeric value there, and
// Dirty (permanently, for this shard) from the first row that did not. The
// classifier runs on its own goroutine, handed each batch after the append
// commits, so it adds nothing to the ingest path.

const (
	// factTypedAfter is how many consecutive conforming rows type a locator.
	factTypedAfter = 1000
	// factMaxLocators caps the locators tracked per source; later ones are
	// simply unknown.
	factMaxLocators = 50
	// factQueueDepth is how many batches may await the classifier. A batch
	// that doesn't fit disables the classifier (see Observe).
	factQueueDepth = 1024
)

// FactState is where a locator is on its way to being typed.
type FactState int8

const (
	FactUnknown FactState = iota
	FactTyped
	FactDirty
)

// FactKey names a locator in a source. Customer is part of the key because
// shards are shared across customers; env so that a stray row in staging
// doesn't downgrade production. Locator is "$N" for awk field N.
type FactKey struct {
	CustomerID int64
	Namespace  string
	Service    string
	Kind       string
	Env        string
	Locator    string
}

// FieldFact is one locator's reportable state. Epoch increases with every
// transition the coordinator needs to hear about (typed, or typed then
// dirty), across all of the shard's facts.
type FieldFact struct {
	FactKey
	State FactState
	Epoch uint64
}

type locatorState struct {
	locator  string // "$N" or ".key"
	state    FactState
	conform  int64
	wasTyped bool // reached Typed once, so the coordinator may hold it
	epoch    uint64
	seen     int64 // src.rows+1 of the last row that held this locator
}

// factSource is what is known about one source's locators. Awk fields and
// top-level jq keys are tracked separately, up to factMaxLocators of each: a
// JSON row also has awk fields (the text of its line), which must not crowd
// out its keys.
type factSource struct {
	rows  int64
	awk   map[int]*locatorState
	jq    map[string]*locatorState
	order []*locatorState // every tracked locator, to find the ones a row lacks
}

type sourceKey struct {
	customerID                    int64
	namespace, service, kind, env string
}

// FieldClassifier learns field facts from the batches a shard ingests. A nil
// *FieldClassifier is valid and holds no facts.
type FieldClassifier struct {
	in   chan []*pb.LogRow
	done chan struct{}
	// inflight counts batches queued or being classified; tests wait on it.
	inflight sync.WaitGroup

	mu      sync.Mutex
	sources map[sourceKey]*factSource
	epoch   uint64
	acked   uint64
	// changed: a classification result differs from the one last persisted
	// (a source or locator started being tracked, or one became typed or
	// dirty). Counting more conforming rows is not a change.
	changed bool
	// persist keeps a snapshot of the results where a roll will find it.
	// Called on the classifier goroutine, only when changed.
	persist  func([]byte) error
	disabled bool
}

// NewFieldClassifier starts a classifier. Stop it with Stop.
func NewFieldClassifier() *FieldClassifier {
	c := &FieldClassifier{
		in:      make(chan []*pb.LogRow, factQueueDepth),
		done:    make(chan struct{}),
		sources: map[sourceKey]*factSource{},
	}
	go c.run()
	return c
}

// Stop ends the classifier's goroutine. Observe after Stop is a no-op.
func (c *FieldClassifier) Stop() {
	if c == nil {
		return
	}
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

// Observe hands the classifier a committed batch. It never blocks. A batch
// that doesn't fit the queue was not classified, so any fact could now be
// wrong: the classifier turns itself off for good and reports nothing, which
// every consumer reads as "unknown".
func (c *FieldClassifier) Observe(rows []*pb.LogRow) {
	if c == nil || len(rows) == 0 {
		return
	}
	// The caller may reuse its slice (the ingest stream does), the rows
	// themselves are not mutated.
	batch := append([]*pb.LogRow(nil), rows...)
	c.inflight.Add(1)
	select {
	case c.in <- batch:
	case <-c.done:
		c.inflight.Done()
	default:
		c.inflight.Done()
		c.disable("classifier queue full")
	}
}

// Disable turns the classifier off, e.g. for a shard that opened with rows
// it has not classified. Rows are append-only and never rescanned: a shard
// whose saved state doesn't match its rows simply has no facts.
func (c *FieldClassifier) Disable(reason string) { c.disable(reason) }

func (c *FieldClassifier) disable(reason string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	already := c.disabled
	c.disabled = true
	c.mu.Unlock()
	if !already {
		weftsdk.Warnf(context.Background(), "field facts disabled: %s", reason)
	}
}

func (c *FieldClassifier) run() {
	for {
		select {
		case <-c.done:
			return
		case batch := <-c.in:
			c.mu.Lock()
			if !c.disabled {
				for _, r := range batch {
					c.classify(r)
				}
			}
			snap, save := c.takeChange()
			c.mu.Unlock()
			if save {
				if err := c.persist(snap); err != nil {
					weftsdk.Warnf(context.Background(), "field facts: not saved: %v", err)
					c.mu.Lock()
					c.changed = true // the next batch tries again
					c.mu.Unlock()
				}
			}
			c.inflight.Done()
		}
	}
}

// classify folds one row in. Caller holds c.mu.
func (c *FieldClassifier) classify(r *pb.LogRow) {
	kind := r.Kind
	if kind == "" {
		kind = "main"
	}
	c.classifyRow(sourceKey{r.CustomerId, r.Namespace, r.Service, kind, r.Env}, awkFields(r.Raw), r.PayloadJson)
}

// awkLocators names the first fields without building a string per row.
var awkLocators = func() []string {
	out := make([]string, 257)
	for i := range out {
		out[i] = "$" + strconv.Itoa(i)
	}
	return out
}()

func awkLocator(n int) string {
	if n < len(awkLocators) {
		return awkLocators[n]
	}
	return "$" + strconv.Itoa(n)
}

// classifyRow folds one row's awk fields and top-level payload keys in.
// Caller holds c.mu.
func (c *FieldClassifier) classifyRow(sk sourceKey, fields []string, payload []byte) {
	src := c.sources[sk]
	if src == nil {
		src = &factSource{awk: map[int]*locatorState{}, jq: map[string]*locatorState{}}
		c.sources[sk] = src
		c.changed = true
	}
	mark := src.rows + 1
	for i, f := range fields {
		n := i + 1
		ls := src.awk[n]
		if ls == nil {
			if ls = src.track(awkLocator(n), len(src.awk)); ls == nil {
				continue
			}
			src.awk[n] = ls
			c.changed = true
		}
		ls.seen = mark
		c.observe(ls, awk.LooksNumeric(f), sk)
	}
	if root := gjson.ParseBytes(payload); root.IsObject() {
		root.ForEach(func(k, v gjson.Result) bool {
			key := k.String()
			ls := src.jq[key]
			if ls == nil {
				if ls = src.track("."+key, len(src.jq)); ls == nil {
					return true
				}
				src.jq[key] = ls
				c.changed = true
			}
			ls.seen = mark
			// jq compares a JSON number as a number and everything else
			// (a string that reads as a number, null, a bool) otherwise.
			c.observe(ls, v.Type == gjson.Number, sk)
			return true
		})
	}
	// A locator this row lacks is not a number there.
	for _, ls := range src.order {
		if ls.seen != mark {
			c.observe(ls, false, sk)
		}
	}
	src.rows++
}

// track starts following a locator the source has not shown before, or
// returns nil if kindCount locators of its kind are already followed. Rows
// before this one lacked it, so it starts dirty unless this is the first row.
func (src *factSource) track(locator string, kindCount int) *locatorState {
	if kindCount >= factMaxLocators {
		return nil
	}
	ls := &locatorState{locator: locator}
	if src.rows > 0 {
		ls.state = FactDirty
	}
	src.order = append(src.order, ls)
	return ls
}

// observe folds one value's verdict into a locator. Caller holds c.mu.
func (c *FieldClassifier) observe(ls *locatorState, numeric bool, sk sourceKey) {
	switch ls.state {
	case FactDirty:
		return
	case FactTyped:
		if !numeric {
			c.epoch++
			c.changed = true
			ls.state, ls.epoch = FactDirty, c.epoch
			weftsdk.Infof(context.Background(), "field fact flipped dirty: customer=%d %s/%s/%s env=%s locator=%s",
				sk.customerID, sk.namespace, sk.service, sk.kind, sk.env, ls.locator)
		}
	default:
		if !numeric {
			ls.state = FactDirty
			c.changed = true
			return
		}
		ls.conform++
		if ls.conform >= factTypedAfter {
			c.changed = true
			c.epoch++
			ls.state, ls.epoch, ls.wasTyped = FactTyped, c.epoch, true
		}
	}
}

// Pending returns the facts the coordinator has not acknowledged: typed
// ones, and ones that were typed and went dirty (the coordinator must drop
// them). A locator that was never typed is not reported; absence means
// unknown.
func (c *FieldClassifier) Pending() []FieldFact {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled {
		return nil
	}
	var out []FieldFact
	for sk, src := range c.sources {
		for _, ls := range src.order {
			if !ls.wasTyped || ls.epoch <= c.acked {
				continue
			}
			out = append(out, FieldFact{
				FactKey: FactKey{sk.customerID, sk.namespace, sk.service, sk.kind, sk.env, ls.locator},
				State:   ls.state,
				Epoch:   ls.epoch,
			})
		}
	}
	return out
}

// Ack records that the coordinator has every fact up to epoch.
func (c *FieldClassifier) Ack(epoch uint64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if epoch > c.acked {
		c.acked = epoch
	}
	c.mu.Unlock()
}

// Drain waits for every batch handed to Observe to be classified, and any
// change it made to be persisted.
func (c *FieldClassifier) Drain() {
	if c != nil {
		c.inflight.Wait()
	}
}

// takeChange is the snapshot to persist, if a result changed since the last
// one. Caller holds c.mu.
func (c *FieldClassifier) takeChange() ([]byte, bool) {
	if c.persist == nil || c.disabled || !c.changed {
		return nil, false
	}
	b, err := c.snapshot()
	if err != nil {
		return nil, false
	}
	c.changed = false
	return b, true
}

// savedFacts is the classification results as they are kept in the shard's
// own database, so a roll (a new instance opening the same volume) carries
// them over. Counts of rows still on their way to being typed are not kept: a
// locator that was part-way starts counting again, which is slower and safe.
type savedFacts struct {
	Epoch   uint64
	Sources []savedSource
}

type savedSource struct {
	CustomerID                    int64
	Namespace, Service, Kind, Env string
	Locators                      []savedLocator
}

type savedLocator struct {
	Locator  string
	State    FactState
	WasTyped bool
	Epoch    uint64
}

// snapshot encodes the results. Caller holds c.mu.
func (c *FieldClassifier) snapshot() ([]byte, error) {
	out := savedFacts{Epoch: c.epoch}
	for sk, src := range c.sources {
		ss := savedSource{CustomerID: sk.customerID, Namespace: sk.namespace, Service: sk.service,
			Kind: sk.kind, Env: sk.env}
		for _, ls := range src.order {
			ss.Locators = append(ss.Locators, savedLocator{ls.locator, ls.state, ls.wasTyped, ls.epoch})
		}
		out.Sources = append(out.Sources, ss)
	}
	return json.Marshal(out)
}

// Restore loads results saved by snapshot into a fresh classifier. A restored
// source counts as having had rows, so a locator it did not save is one the
// earlier rows lacked, and stays dirty.
func (c *FieldClassifier) Restore(b []byte) error {
	var in savedFacts
	if err := json.Unmarshal(b, &in); err != nil {
		return fmt.Errorf("saved field facts: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch = in.Epoch
	for _, ss := range in.Sources {
		src := &factSource{rows: 1, awk: map[int]*locatorState{}, jq: map[string]*locatorState{}}
		for _, sl := range ss.Locators {
			ls := &locatorState{locator: sl.Locator, state: sl.State, wasTyped: sl.WasTyped, epoch: sl.Epoch}
			src.order = append(src.order, ls)
			switch {
			case strings.HasPrefix(sl.Locator, "$"):
				n, err := strconv.Atoi(sl.Locator[1:])
				if err != nil {
					return fmt.Errorf("saved field facts: locator %q", sl.Locator)
				}
				src.awk[n] = ls
			case strings.HasPrefix(sl.Locator, "."):
				src.jq[sl.Locator[1:]] = ls
			default:
				return fmt.Errorf("saved field facts: locator %q", sl.Locator)
			}
		}
		c.sources[sourceKey{ss.CustomerID, ss.Namespace, ss.Service, ss.Kind, ss.Env}] = src
	}
	c.changed = false
	return nil
}

// SetPersist installs the function that keeps the results. It must be set
// before the classifier sees a row.
func (c *FieldClassifier) SetPersist(f func([]byte) error) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.persist = f
	c.mu.Unlock()
}
