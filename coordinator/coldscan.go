package coordinator

import (
	"context"
	"sync"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/compute"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
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
	cache   map[int64]*coldScanTotal
	now     func() time.Time
	// load reads an org's total for [start, end); the store's QueryColdScan.
	load func(org int64, start, end time.Time) (int64, error)
}

type coldScanKey struct {
	org int64
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
	Org  int64
	Day  time.Time
	Rows int64
}

// NewColdScanLedger builds a ledger that reads totals through st.
func NewColdScanLedger(st *store.Store) *ColdScanLedger {
	return newColdScanLedger(time.Now, func(org int64, start, end time.Time) (int64, error) {
		return st.QueryColdScan(store.OrgID(org), start, end)
	})
}

func newColdScanLedger(now func() time.Time, load func(int64, time.Time, time.Time) (int64, error)) *ColdScanLedger {
	return &ColdScanLedger{
		pending: map[coldScanKey]int64{},
		cache:   map[int64]*coldScanTotal{},
		now:     now,
		load:    load,
	}
}

func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Record adds lines scanned for an org, today.
func (l *ColdScanLedger) Record(org int64, rows int64) {
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
func (l *ColdScanLedger) Used(org int64) (int64, error) {
	now := l.now()
	start, end := store.ColdScanPeriod(now)

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
// query when enforcement is off or the total cannot be read, go through: a
// metering gap costs us a little, a wrong refusal costs a customer a query.
func (c *Coordinator) checkColdScanBudget(ctx context.Context, org store.OrgID, tier store.Tier, descs []*pb.ShardDescriptor) error {
	if c.ColdScans == nil || !c.ColdScanEnforce || org == 0 || !hasColdShard(descs) {
		return nil
	}
	allowance := store.ColdScanAllowance(tier)
	used, err := c.ColdScans.Used(int64(org))
	if err != nil {
		weftsdk.Errorf(ctx, "cold scan: reading org %d's month total: %v; allowing the query", org, err)
		return nil
	}
	if used < allowance {
		return nil
	}
	_, resets := store.ColdScanPeriod(c.ColdScans.now())
	weftsdk.Infof(ctx, "cold scan: org %d refused: used %d of %d lines", org, used, allowance)
	return &store.ColdScanBudgetError{Used: used, Allowance: allowance, ResetsAt: resets}
}

// executeCompute is where every query reaches compute. It refuses a query the
// cold budget does not cover, and counts what the query scans once it ends.
func (c *Coordinator) executeCompute(ctx context.Context, org store.OrgID, tier store.Tier, req *pb.ExecuteQueryRequest) (compute.LineStream, error) {
	if err := c.checkColdScanBudget(ctx, org, tier, req.Shards); err != nil {
		return nil, err
	}
	stream, err := c.Compute.ExecuteQuery(ctx, req)
	if err != nil || c.ColdScans == nil || org == 0 {
		return stream, err
	}
	return &meteredStream{LineStream: stream, record: func(rows int64) {
		c.ColdScans.Record(int64(org), rows)
		weftsdk.Infof(ctx, "cold scan: org=%d rows=%d", org, rows)
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

// flushColdScans appends each drained delta as its own row.
func (c *Coordinator) flushColdScans(st *store.Store) {
	if c.ColdScans == nil {
		return
	}
	for _, e := range c.ColdScans.Drain() {
		if err := st.AddColdScan(e.Org, e.Day, e.Rows); err != nil {
			weftsdk.Errorf(context.Background(), "cold scan flush error: org=%d day=%s: %v", e.Org, e.Day.Format("2006-01-02"), err)
		}
	}
}
