package coordinator

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CustomerUsageStats tracks per-customer bytes ingested, bucketed by hour.
// The drain loop calls Record for every row; a background goroutine
// periodically calls Drain to flush accumulated deltas to PostgreSQL —
// completed hours are removed entirely, the current hour is reset to
// zero but kept, since it's still accumulating.
type CustomerUsageStats struct {
	mu     sync.Mutex
	counts map[string]*atomic.Int64 // key: "customer\x00YYYY-MM-DDTHH"
}

// NewCustomerUsageStats creates an empty usage tracker.
func NewCustomerUsageStats() *CustomerUsageStats {
	return &CustomerUsageStats{
		counts: make(map[string]*atomic.Int64),
	}
}

func usageKey(customer string, hour time.Time) string {
	return customer + "\x00" + hour.Format("2006-01-02T15")
}

func parseUsageKey(key string) (customer, hourStr string) {
	i := strings.IndexByte(key, 0)
	if i < 0 {
		return key, ""
	}
	return key[:i], key[i+1:]
}

// Record adds bytes to the current hour's bucket for a customer.
func (u *CustomerUsageStats) Record(customer string, bytes int64) {
	hour := time.Now().UTC().Truncate(time.Hour)
	key := usageKey(customer, hour)

	u.mu.Lock()
	counter, ok := u.counts[key]
	if !ok {
		counter = &atomic.Int64{}
		u.counts[key] = counter
	}
	u.mu.Unlock()

	counter.Add(bytes)
}

// UsageEntry is one flush's worth of usage data: bytes accumulated for a
// customer/hour since the last Drain, ready to append to PostgreSQL.
type UsageEntry struct {
	Customer string
	Hour     time.Time
	Bytes    int64
}

// Drain returns every bucket's delta since the last call and resets each
// counter to zero. Buckets for hours before the current one are removed
// entirely (that hour will never accumulate more); the current hour's
// bucket is reset but kept, since Record may still be adding to it.
// Safe to call frequently — each call only ever returns the delta since
// it was last drained, so the caller can append it straight to
// usage_hourly without double-counting on the next tick.
func (u *CustomerUsageStats) Drain() []UsageEntry {
	currentHour := time.Now().UTC().Truncate(time.Hour).Format("2006-01-02T15")

	u.mu.Lock()
	defer u.mu.Unlock()

	var entries []UsageEntry
	for key, counter := range u.counts {
		customer, hourStr := parseUsageKey(key)
		hour, err := time.Parse("2006-01-02T15", hourStr)
		if err != nil {
			continue
		}
		bytes := counter.Swap(0)
		if bytes > 0 {
			entries = append(entries, UsageEntry{
				Customer: customer,
				Hour:     hour,
				Bytes:    bytes,
			})
		}
		if hourStr != currentHour {
			delete(u.counts, key)
		}
	}
	return entries
}

// CurrentHour returns the bytes counted so far in the current (unflushed)
// hour for a customer. Used by GetUsage to include not-yet-persisted data.
func (u *CustomerUsageStats) CurrentHour(customer string) int64 {
	hour := time.Now().UTC().Truncate(time.Hour)
	key := usageKey(customer, hour)

	u.mu.Lock()
	counter, ok := u.counts[key]
	u.mu.Unlock()

	if !ok {
		return 0
	}
	return counter.Load()
}
