package coordinator

import (
	"context"
	"sync"
	"time"

	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/tenancy"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/w3ft-net/weft/compute"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// coldScanCacheTTL is how long an org's month total is trusted before it is
// read again. Another coordinator's scans are invisible for up to this long,
// which bounds how far a budget can be overshot (docs/plans/cold-scan-cap.md).
const coldScanCacheTTL = 30 * time.Second

// ColdScanLedger counts the lines each org's queries scanned from cold
// storage. Record accumulates in memory; Drain hands the deltas to a flusher
// that appends them to cold_scan_daily; Used answers "how much this month",
// from a short-lived read of the table plus what this coordinator has counted
// since. It follows CustomerUsageStats: append-only deltas, so two
// coordinators never race on a shared row.
type ColdScanLedger struct {
	mu      sync.Mutex
	pending map[coldScanKey]int64
	cache   map[shape.Added]*coldScanTotal
	now     func() time.Time
	// load reads an org's total for [start, end); the store's QueryColdScan.
	load func(org shape.Added, start, end time.Time) (int64, error)
}

type coldScanKey struct {
	org shape.Added
	day time.Time // UTC midnight
}

// coldScanTotal is an org's month total as last read, plus what this
// coordinator has flushed to the table since (which that read cannot include).
type coldScanTotal struct {
	base, flushed int64
	at            time.Time
	month         time.Time // UTC first of the month the base covers
}

// ColdScanEntry is one flush's worth of lines for an org and day.
type ColdScanEntry struct {
	Org  shape.Added
	Day  time.Time
	Rows int64
}

// NewColdScanLedger builds a ledger that reads an org's total for [start, end)
// through load.
func NewColdScanLedger(load func(org shape.Added, start, end time.Time) (int64, error)) *ColdScanLedger {
	return newColdScanLedger(time.Now, load)
}

func newColdScanLedger(now func() time.Time, load func(shape.Added, time.Time, time.Time) (int64, error)) *ColdScanLedger {
	return &ColdScanLedger{
		pending: map[coldScanKey]int64{},
		cache:   map[shape.Added]*coldScanTotal{},
		now:     now,
		load:    load,
	}
}

func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Record adds lines scanned for an org, today.
func (l *ColdScanLedger) Record(org shape.Added, rows int64) {
	if rows <= 0 {
		return
	}
	l.mu.Lock()
	l.pending[coldScanKey{org, utcDay(l.now())}] += rows
	l.mu.Unlock()
}

// Drain returns every unflushed delta and clears them, noting each in the
// org's cached total so Used does not lose it between the flush and the next
// read of the table.
func (l *ColdScanLedger) Drain() []ColdScanEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []ColdScanEntry
	for k, rows := range l.pending {
		out = append(out, ColdScanEntry{Org: k.org, Day: k.day, Rows: rows})
		if c := l.cache[k.org]; c != nil && sameMonth(c.month, k.day) {
			c.flushed += rows
		}
	}
	clear(l.pending)
	return out
}

func sameMonth(monthStart, day time.Time) bool {
	return day.Year() == monthStart.Year() && day.Month() == monthStart.Month()
}

// Used is the lines an org has scanned this month: the table as last read
// (at most coldScanCacheTTL old), plus this coordinator's flushed and pending
// scans since. It errors only when the table cannot be read, and then with
// nothing to say about the total.
func (l *ColdScanLedger) Used(org shape.Added) (int64, error) {
	now := l.now()
	start, end := tenancy.ColdScanPeriod(now)

	l.mu.Lock()
	c := l.cache[org]
	fresh := c != nil && c.month.Equal(start) && now.Sub(c.at) < coldScanCacheTTL
	l.mu.Unlock()

	if !fresh {
		base, err := l.load(org, start, end)
		if err != nil {
			return 0, err
		}
		l.mu.Lock()
		// What this coordinator holds unflushed is not in the table yet;
		// what it flushed before this read may or may not be. Counting the
		// first as pending and none of the second errs low by at most one
		// flush interval of this coordinator's own scans.
		l.cache[org] = &coldScanTotal{base: base, at: now, month: start}
		l.mu.Unlock()
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	c = l.cache[org]
	used := c.base + c.flushed
	for k, rows := range l.pending {
		if k.org == org && sameMonth(start, k.day) {
			used += rows
		}
	}
	return used, nil
}

// hasColdShard reports whether any shard in the set is cold storage.
func hasColdShard(descs []*pb.ShardDescriptor) bool {
	for _, d := range descs {
		if d.LambdaUrl != "" || d.ParquetPath != "" {
			return true
		}
	}
	return false
}

// checkColdScanBudget refuses a query that needs a cold shard once the org's
// month of cold scanning is used up. Queries over hot shards only, and any
// query when the total cannot be read, go through: a metering gap costs us a
// little, a wrong refusal costs a customer a query.
func (c *Coordinator) checkColdScanBudget(ctx context.Context, owner shape.Added, limits tenancy.Limits, descs []*pb.ShardDescriptor) error {
	if c.ColdScans == nil || NoTenant(owner) || limits.ColdScanLines == nil || !hasColdShard(descs) {
		return nil
	}
	allowance := *limits.ColdScanLines
	used, err := c.ColdScans.Used(owner)
	if err != nil {
		weftsdk.Errorf(ctx, "cold scan: reading %+v's month total: %v; allowing the query", owner, err)
		return nil
	}
	if used < allowance {
		return nil
	}
	_, resets := tenancy.ColdScanPeriod(c.ColdScans.now())
	weftsdk.Infof(ctx, "cold scan: %+v refused: used %d of %d lines", owner, used, allowance)
	return &tenancy.ColdScanBudgetError{Used: used, Allowance: allowance, ResetsAt: resets}
}

// executeCompute is where every query reaches compute. It refuses a query the
// cold budget does not cover, and counts what the query scans once it ends.
func (c *Coordinator) executeCompute(ctx context.Context, owner shape.Added, limits tenancy.Limits, req *pb.ExecuteQueryRequest) (compute.LineStream, error) {
	if err := c.checkColdScanBudget(ctx, owner, limits, req.Shards); err != nil {
		return nil, err
	}
	stream, err := c.Compute.ExecuteQuery(ctx, req)
	if err != nil || c.ColdScans == nil || NoTenant(owner) {
		return stream, err
	}
	return &meteredStream{LineStream: stream, record: func(rows int64) {
		c.ColdScans.Record(owner, rows)
		weftsdk.Infof(ctx, "cold scan: owner=%+v rows=%d", owner, rows)
	}}, nil
}

// meteredStream records the lines a query scanned from cold storage, once, when
// the stream ends or is closed. A stream that does not know (a compute from
// before the count) records nothing.
type meteredStream struct {
	compute.LineStream
	once   sync.Once
	record func(rows int64)
}

func (s *meteredStream) Next(ctx context.Context) (compute.Line, bool, error) {
	line, ok, err := s.LineStream.Next(ctx)
	if !ok || err != nil {
		s.finish()
	}
	return line, ok, err
}

func (s *meteredStream) Close() error {
	s.finish()
	return s.LineStream.Close()
}

func (s *meteredStream) finish() {
	s.once.Do(func() {
		if sc, ok := s.LineStream.(compute.ScanCounter); ok {
			if rows := sc.RowsScanned(); rows > 0 {
				s.record(rows)
			}
		}
	})
}
