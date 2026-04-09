package coordinator

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CustomerUsageStats tracks per-customer bytes ingested, bucketed by hour.
// The drain loop calls Record for every row; a background goroutine
// periodically calls DrainCompleted to flush finished hours to PostgreSQL.
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

// UsageEntry is a completed hour's usage data ready for DB flush.
type UsageEntry struct {
	Customer string
	Hour     time.Time
	Bytes    int64
}

// DrainCompleted returns and removes all buckets for hours before the current
// hour. The current hour is kept in memory (still accumulating).
func (u *CustomerUsageStats) DrainCompleted() []UsageEntry {
	currentHour := time.Now().UTC().Truncate(time.Hour).Format("2006-01-02T15")

	u.mu.Lock()
	defer u.mu.Unlock()

	var entries []UsageEntry
	for key, counter := range u.counts {
		_, hourStr := parseUsageKey(key)
		if hourStr == currentHour {
			continue // still accumulating
		}
		customer, _ := parseUsageKey(key)
		hour, err := time.Parse("2006-01-02T15", hourStr)
		if err != nil {
			continue
		}
		bytes := counter.Load()
		if bytes > 0 {
			entries = append(entries, UsageEntry{
				Customer: customer,
				Hour:     hour,
				Bytes:    bytes,
			})
		}
		delete(u.counts, key)
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
