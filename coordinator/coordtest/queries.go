//go:build test

package coordtest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"
	"github.com/w3ft-net/weft/backends/shardstore"
	"github.com/w3ft-net/weft/compute"
	"github.com/w3ft-net/weft/compute/computetest"
	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/logdb"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// Queries runs the suites of log queries over in-process DuckDB shards: the
// shell commands pushed down to shard SQL, the stages compute runs, and the time
// range and shard routing. Each test gets a fresh Subject.
func Queries(t *testing.T, newSubject func(t *testing.T) Subject) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, *queryFixture)
	}{
		{"AwkLevelFilter", queryAwkLevelFilter},
		{"GrepTimeout", queryGrepTimeout},
		{"JqSelectStatus500", queryJqSelectStatus500},
		{"HeadLimit", queryHeadLimit},
		{"TailLimit", queryTailLimit},
		{"TimeRangeScopingShard0Only", queryShard0Only},
		{"TimeRangeScopingShard1Only", queryShard1Only},
		{"GroupIsolation", queryGroupIsolation},
		{"WarpLogAppFilter", queryAppFilter},
		{"ResultsOrderedByTimestamp", queryOrderedByTimestamp},
		{"MergesAcrossShards", queryMergesAcrossShards},
		{"ComputeSort", querySort},
		{"ComputeWcCount", queryWcCount},
		{"ComputeSortUniqCount", querySortUniqCount},
		{"AwkFieldExtract", queryAwkFieldExtract},
		{"SendsTheFieldFormToCompute", querySendsFieldForm},
		{"NoStagesRoutesThroughCompute", queryNoStagesThroughCompute},
		{"TraceIDReachesComputeCall", queryTraceIDReachesCompute},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, newQueryFixture(t, newSubject(t))) })
	}
}

// queryFixture is a coordinator over two in-process shards, each an hour of
// realistic logs of the namespaces vimgym, callbox and warp: shard-0 covers
// [t0, t1) and shard-1 [t1, t2). The times are relative to now, because a
// query's range is floored at now less the plan's retention, and a fixed date
// would one day fall outside it.
type queryFixture struct {
	s          Subject
	c          *coordinator.Coordinator
	t0, t1, t2 time.Time
	namespaces map[string]uint
	bucketID   int64
}

// register makes e a shard of the coordinator: compute runs against its
// in-process handle without dialing, and the registry sees it. Production
// persists the shard and stamps the services it serves at christen; SeedShard
// skips that path, so this is its stand-in, and the read router resolves
// through it.
func (f *queryFixture) register(t *testing.T, e *logdb.Shard) {
	t.Helper()
	f.registerServing(t, e, "vimgym", "callbox")
}

// registerServing is register for a shard serving the given services, each a
// namespace or "namespace/service".
func (f *queryFixture) registerServing(t *testing.T, e *logdb.Shard, services ...string) {
	t.Helper()
	if eng, ok := f.c.Compute.(*compute.Engine); ok && e.Live != nil {
		eng.RegisterResolver(computetest.NewInProcessResolver(e.ID, e.Live))
	}
	logdb.SeedShard(f.c.Provisioner, e)
	if e.BucketID == 0 {
		e.BucketID = f.bucketID
	}
	if err := f.s.Shards.SaveShard(&e.ShardRow); err != nil {
		t.Fatalf("persist %s: %v", e.ID, err)
	}
	var stamps []shardstore.ShardService
	for _, spec := range services {
		ns, svc, ok := strings.Cut(spec, "/")
		if !ok {
			svc = ns
		}
		stamps = append(stamps, shardstore.ShardService{ShardID: e.ID, NamespaceID: f.namespaces[ns], Service: svc})
	}
	if err := f.s.Shards.StampShardServices(e.ID, stamps); err != nil {
		t.Fatalf("stamp %s: %v", e.ID, err)
	}
	e.Services = append(e.Services, stamps...)
}

func (f *queryFixture) full() coordinator.TimeRange {
	return coordinator.TimeRange{Start: f.t0, End: f.t2}
}

func payload(m map[string]any) []byte {
	b, _ := json.Marshal(m)
	return b
}

func newQueryFixture(t *testing.T, s Subject) *queryFixture {
	t.Helper()
	f := newBareFixture(t, s, coordinator.CoordinatorConfig{})
	f.seedShards(t, nil)
	return f
}

// newBareFixture is the coordinator, the namespaces and the bucket, with no
// shard yet: a test that brings its own shards registers them with register.
func newBareFixture(t *testing.T, s Subject, cfg ...coordinator.CoordinatorConfig) *queryFixture {
	t.Helper()
	f := &queryFixture{s: s}
	f.t0 = time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	f.t1 = f.t0.Add(time.Hour)
	f.t2 = f.t1.Add(time.Hour)

	f.namespaces = map[string]uint{}
	for _, ns := range []string{"vimgym", "callbox", "warp"} {
		n, err := s.Sources.FindOrCreateNamespace(s.Owner, ns)
		if err != nil {
			t.Fatal(err)
		}
		f.namespaces[ns] = n.ID
	}
	var c coordinator.CoordinatorConfig
	if len(cfg) > 0 {
		c = cfg[0]
	}
	f.c = s.coordinator(t, c)

	bucket, ok, err := s.Shards.GetBucketByLabel("default")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		bucket = shardstore.Bucket{Label: "default", TargetIngestingCount: 1}
		if err := s.Shards.SaveBucket(&bucket); err != nil {
			t.Fatal(err)
		}
	}

	f.bucketID = bucket.ID
	return f
}

// seedShards registers the two shards of realistic logs the suites query. wrap,
// when set, is the handle the coordinator reaches each by instead of the
// in-process one (a gRPC client of it) and the address it is served at.
func (f *queryFixture) seedShards(t *testing.T, wrap func(*testing.T, *shard.DuckDBShard) (shard.Shard, string)) {
	t.Helper()
	s := f.s
	bucket := shardstore.Bucket{ID: f.bucketID}
	for _, sh := range []struct {
		id         string
		start, end time.Time
		rows       []*pb.LogRow
		other      []*pb.LogRow
	}{
		{"shard-0", f.t0, f.t1, queryShard0Rows(f.t0), queryOtherRows(f.t0)},
		{"shard-1", f.t1, f.t2, queryShard1Rows(f.t1), nil},
	} {
		live, err := shard.NewDuckDBShard(sh.id, "")
		if err != nil {
			t.Fatalf("create %s: %v", sh.id, err)
		}
		t.Cleanup(func() { live.Close() })
		for _, r := range sh.rows {
			if s.Stamp != nil {
				s.Stamp(r)
			}
		}
		if err := live.Ingest(context.Background(), sh.rows, nil); err != nil {
			t.Fatalf("ingest %s: %v", sh.id, err)
		}
		if s.StampOther != nil {
			for _, r := range sh.other {
				s.StampOther(r)
			}
			if err := live.Ingest(context.Background(), sh.other, nil); err != nil {
				t.Fatalf("ingest %s's other group: %v", sh.id, err)
			}
		}

		var handle shard.Shard = live
		var addr string
		if wrap != nil {
			handle, addr = wrap(t, live)
		}
		f.register(t, &logdb.Shard{
			ShardRow: logdb.ShardRow{ID: sh.id, WindowStart: sh.start, WindowEnd: sh.end, State: logdb.StateIngesting, BucketID: bucket.ID, Addr: addr, CreatedAt: time.Now()},
			Live:     handle,
		})
	}
}

// exec runs line over tr as the subject's owner and returns what it produced.
func (f *queryFixture) exec(t *testing.T, line string, tr coordinator.TimeRange) *coordinator.DrainedResult {
	t.Helper()
	res, err := f.c.Execute(context.Background(), line, tr, f.s.PrincipalOf(""))
	if err != nil {
		t.Fatal(err)
	}
	d, err := res.Drain(context.Background())
	if err != nil {
		t.Fatalf("drain %q: %v", line, err)
	}
	return d
}

func nonEmptyLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func queryShard0Rows(base time.Time) []*pb.LogRow {
	return []*pb.LogRow{
		{Raw: "2026-03-28T10:00:01 ERROR vimgym i-001 connection timeout", TsUnixNs: base.Add(1 * time.Second).UnixNano(), Level: "ERROR", Namespace: "vimgym", Instance: "i-001", PayloadJson: payload(map[string]any{"status": 500, "action": "login", "user_id": "u1"})},
		{Raw: "2026-03-28T10:00:05 INFO vimgym i-001 request completed", TsUnixNs: base.Add(5 * time.Second).UnixNano(), Level: "INFO", Namespace: "vimgym", Instance: "i-001", PayloadJson: payload(map[string]any{"status": 200, "action": "list", "duration_ms": 45})},
		{Raw: "2026-03-28T10:01:00 WARN callbox i-002 slow query detected", TsUnixNs: base.Add(1 * time.Minute).UnixNano(), Level: "WARN", Namespace: "callbox", Instance: "i-002", PayloadJson: payload(map[string]any{"status": 200, "action": "search", "duration_ms": 1500})},
		{Raw: "2026-03-28T10:02:00 ERROR vimgym i-001 null pointer exception", TsUnixNs: base.Add(2 * time.Minute).UnixNano(), Level: "ERROR", Namespace: "vimgym", Instance: "i-001", PayloadJson: payload(map[string]any{"status": 500, "action": "update", "user_id": "u2"})},
		{Raw: "2026-03-28T10:03:00 INFO callbox i-002 health check ok", TsUnixNs: base.Add(3 * time.Minute).UnixNano(), Level: "INFO", Namespace: "callbox", Instance: "i-002", PayloadJson: payload(map[string]any{"status": 200, "action": "health"})},
		{Raw: "2026-03-28T10:05:00 ERROR callbox i-002 connection timeout to database", TsUnixNs: base.Add(5 * time.Minute).UnixNano(), Level: "ERROR", Namespace: "callbox", Instance: "i-002", PayloadJson: payload(map[string]any{"status": 503, "action": "query"})},
		{Raw: "2026-03-28T10:10:00 INFO vimgym i-003 user signup completed", TsUnixNs: base.Add(10 * time.Minute).UnixNano(), Level: "INFO", Namespace: "vimgym", Instance: "i-003", PayloadJson: payload(map[string]any{"status": 201, "action": "signup", "user_id": "u3"})},
		{Raw: "2026-03-28T10:15:00 DEBUG vimgym i-001 cache miss for key session:u1", TsUnixNs: base.Add(15 * time.Minute).UnixNano(), Level: "DEBUG", Namespace: "vimgym", Instance: "i-001", PayloadJson: payload(map[string]any{"status": 200, "action": "cache_lookup"})},
		{Raw: "2026-03-28T10:20:00 ERROR vimgym i-003 timeout waiting for response", TsUnixNs: base.Add(20 * time.Minute).UnixNano(), Level: "ERROR", Namespace: "vimgym", Instance: "i-003", PayloadJson: payload(map[string]any{"status": 504, "action": "proxy"})},
		{Raw: "2026-03-28T10:25:00 INFO callbox i-002 batch job completed 150 records", TsUnixNs: base.Add(25 * time.Minute).UnixNano(), Level: "INFO", Namespace: "callbox", Instance: "i-002", PayloadJson: payload(map[string]any{"status": 200, "action": "batch", "count": 150})},
		{Raw: "2026-03-28T10:30:00 WARN vimgym i-001 high memory usage 85%", TsUnixNs: base.Add(30 * time.Minute).UnixNano(), Level: "WARN", Namespace: "vimgym", Instance: "i-001", PayloadJson: payload(map[string]any{"status": 200, "action": "monitor", "memory_pct": 85})},
		{Raw: "2026-03-28T10:35:00 INFO vimgym i-001 request completed", TsUnixNs: base.Add(35 * time.Minute).UnixNano(), Level: "INFO", Namespace: "vimgym", Instance: "i-001", PayloadJson: payload(map[string]any{"status": 200, "action": "list", "duration_ms": 32})},
		{Raw: "2026-03-28T10:40:00 ERROR callbox i-002 authentication failed for group acme", TsUnixNs: base.Add(40 * time.Minute).UnixNano(), Level: "ERROR", Namespace: "callbox", Instance: "i-002", PayloadJson: payload(map[string]any{"status": 401, "action": "auth"})},
		{Raw: "2026-03-28T10:45:00 INFO vimgym i-003 deploy v2.3.1 started", TsUnixNs: base.Add(45 * time.Minute).UnixNano(), Level: "INFO", Namespace: "vimgym", Instance: "i-003", PayloadJson: payload(map[string]any{"status": 200, "action": "deploy", "version": "v2.3.1"})},
		{Raw: "2026-03-28T10:50:00 INFO callbox i-002 connection pool resized to 20", TsUnixNs: base.Add(50 * time.Minute).UnixNano(), Level: "INFO", Namespace: "callbox", Instance: "i-002", PayloadJson: payload(map[string]any{"status": 200, "action": "pool_resize", "size": 20})},
	}
}

func queryShard1Rows(base time.Time) []*pb.LogRow {
	return []*pb.LogRow{
		{Raw: "2026-03-28T11:00:01 INFO vimgym i-001 deploy v2.3.1 completed", TsUnixNs: base.Add(1 * time.Second).UnixNano(), Level: "INFO", Namespace: "vimgym", Instance: "i-001", PayloadJson: payload(map[string]any{"status": 200, "action": "deploy", "version": "v2.3.1"})},
		{Raw: "2026-03-28T11:01:00 ERROR vimgym i-001 500 internal server error on /api/users", TsUnixNs: base.Add(1 * time.Minute).UnixNano(), Level: "ERROR", Namespace: "vimgym", Instance: "i-001", PayloadJson: payload(map[string]any{"status": 500, "action": "api", "path": "/api/users"})},
		{Raw: "2026-03-28T11:05:00 WARN callbox i-002 disk usage above 80%", TsUnixNs: base.Add(5 * time.Minute).UnixNano(), Level: "WARN", Namespace: "callbox", Instance: "i-002", PayloadJson: payload(map[string]any{"status": 200, "action": "monitor", "disk_pct": 82})},
		{Raw: "2026-03-28T11:10:00 INFO vimgym i-003 cache warmed 2500 keys", TsUnixNs: base.Add(10 * time.Minute).UnixNano(), Level: "INFO", Namespace: "vimgym", Instance: "i-003", PayloadJson: payload(map[string]any{"status": 200, "action": "cache_warm", "keys": 2500})},
		{Raw: "2026-03-28T11:15:00 ERROR callbox i-002 timeout connecting to redis", TsUnixNs: base.Add(15 * time.Minute).UnixNano(), Level: "ERROR", Namespace: "callbox", Instance: "i-002", PayloadJson: payload(map[string]any{"status": 503, "action": "cache"})},
		{Raw: "2026-03-28T11:20:00 INFO vimgym i-001 request completed", TsUnixNs: base.Add(20 * time.Minute).UnixNano(), Level: "INFO", Namespace: "vimgym", Instance: "i-001", PayloadJson: payload(map[string]any{"status": 200, "action": "list", "duration_ms": 28})},
		{Raw: "2026-03-28T11:25:00 WARN vimgym i-003 request latency spike p99=2.3s", TsUnixNs: base.Add(25 * time.Minute).UnixNano(), Level: "WARN", Namespace: "vimgym", Instance: "i-003", PayloadJson: payload(map[string]any{"status": 200, "action": "monitor", "p99_ms": 2300})},
		{Raw: "2026-03-28T11:30:00 ERROR vimgym i-001 connection timeout to upstream", TsUnixNs: base.Add(30 * time.Minute).UnixNano(), Level: "ERROR", Namespace: "vimgym", Instance: "i-001", PayloadJson: payload(map[string]any{"status": 504, "action": "proxy"})},
		{Raw: "2026-03-28T11:40:00 INFO callbox i-002 health check ok", TsUnixNs: base.Add(40 * time.Minute).UnixNano(), Level: "INFO", Namespace: "callbox", Instance: "i-002", PayloadJson: payload(map[string]any{"status": 200, "action": "health"})},
		{Raw: "2026-03-28T11:50:00 INFO vimgym i-003 graceful shutdown initiated", TsUnixNs: base.Add(50 * time.Minute).UnixNano(), Level: "INFO", Namespace: "vimgym", Instance: "i-003", PayloadJson: payload(map[string]any{"status": 200, "action": "shutdown"})},
	}
}

func queryOtherRows(t0 time.Time) []*pb.LogRow {
	return []*pb.LogRow{
		{Raw: "2026-03-28T10:00:30 ERROR vimgym i-999 should not appear", TsUnixNs: t0.Add(30 * time.Second).UnixNano(), Level: "ERROR", Namespace: "vimgym", Instance: "i-999", PayloadJson: payload(map[string]any{"status": 500})},
		{Raw: "2026-03-28T10:01:30 INFO callbox i-999 should not appear", TsUnixNs: t0.Add(90 * time.Second).UnixNano(), Level: "INFO", Namespace: "callbox", Instance: "i-999", PayloadJson: payload(map[string]any{"status": 200})},
	}
}

func queryAwkLevelFilter(t *testing.T, f *queryFixture) {
	rows := f.exec(t, `cat vimgym.log | awk 'LEVEL == "ERROR"'`, f.full()).Rows
	for _, r := range rows {
		if !strings.Contains(r.Raw, "vimgym") || !strings.Contains(r.Raw, "ERROR") {
			t.Errorf("expected a vimgym ERROR row, got %q", r.Raw)
		}
	}
	// Shard 0 has vimgym ERROR lines: 1, 4, 9. Shard 1 has: 17, 23. Total 5.
	if len(rows) != 5 {
		t.Errorf("expected 5 rows, got %d", len(rows))
	}
}

func queryGrepTimeout(t *testing.T, f *queryFixture) {
	rows := f.exec(t, `cat warp.log | grep timeout`, f.full()).Rows
	for _, r := range rows {
		if !strings.Contains(strings.ToLower(r.Raw), "timeout") {
			t.Errorf("expected raw to contain 'timeout', got %q", r.Raw)
		}
	}
	// "timeout" appears in lines: 1, 6, 9 (shard 0), 20, 23 (shard 1) = 5
	if len(rows) != 5 {
		t.Errorf("expected 5 timeout rows, got %d", len(rows))
		for _, r := range rows {
			t.Logf("  %s", r.Raw)
		}
	}
}

func queryJqSelectStatus500(t *testing.T, f *queryFixture) {
	rows := f.exec(t, `cat vimgym.log | jq 'select(.status == 500)'`, f.full()).Rows
	for _, r := range rows {
		if !strings.Contains(r.Raw, "vimgym") {
			t.Errorf("expected raw to contain 'vimgym', got %q", r.Raw)
		}
	}
	// status=500 in vimgym: lines 1, 4 (shard 0), 17 (shard 1) = 3
	if len(rows) != 3 {
		t.Errorf("expected 3 rows, got %d", len(rows))
	}
}

func queryHeadLimit(t *testing.T, f *queryFixture) {
	rows := f.exec(t, `head -3 vimgym.log`, f.full()).Rows
	if len(rows) > 3 {
		t.Errorf("expected at most 3 rows, got %d", len(rows))
	}
	for _, r := range rows {
		if !strings.Contains(r.Raw, "vimgym") {
			t.Errorf("expected raw to contain 'vimgym', got %q", r.Raw)
		}
	}
}

func queryTailLimit(t *testing.T, f *queryFixture) {
	rows := f.exec(t, `tail -3 vimgym.log`, f.full()).Rows
	if len(rows) > 3 {
		t.Errorf("expected at most 3 rows, got %d", len(rows))
	}
	for _, r := range rows {
		if !strings.Contains(r.Raw, "vimgym") {
			t.Errorf("expected raw to contain 'vimgym', got %q", r.Raw)
		}
	}
}

func queryShard0Only(t *testing.T, f *queryFixture) {
	rows := f.exec(t, `cat vimgym.log | awk 'LEVEL == "ERROR"'`, coordinator.TimeRange{Start: f.t0, End: f.t1}).Rows
	// Shard 0 vimgym ERROR: lines 1, 4, 9, all timestamped 10:xx.
	if len(rows) != 3 {
		t.Errorf("expected 3 rows from shard 0, got %d", len(rows))
	}
	for _, r := range rows {
		if !strings.Contains(r.Raw, "T10:") {
			t.Errorf("got row outside shard 0 range: %q", r.Raw)
		}
	}
}

func queryShard1Only(t *testing.T, f *queryFixture) {
	rows := f.exec(t, `cat vimgym.log | awk 'LEVEL == "ERROR"'`, coordinator.TimeRange{Start: f.t1, End: f.t2}).Rows
	// Shard 1 vimgym ERROR: lines 17, 23, both timestamped 11:xx.
	if len(rows) != 2 {
		t.Errorf("expected 2 rows from shard 1, got %d", len(rows))
	}
	for _, r := range rows {
		if !strings.Contains(r.Raw, "T11:") {
			t.Errorf("got row outside shard 1 range: %q", r.Raw)
		}
	}
}

// Another group's rows share the shard and never reach the query. A build with
// one group has no other to leak, and skips this.
func queryGroupIsolation(t *testing.T, f *queryFixture) {
	if f.s.StampOther == nil {
		t.Skip("the build has one group")
	}
	for _, r := range f.exec(t, `cat vimgym.log`, f.full()).Rows {
		if strings.Contains(r.Raw, "should not appear") {
			t.Fatal("group isolation broken: another group's row leaked through")
		}
	}
}

func queryAppFilter(t *testing.T, f *queryFixture) {
	rows := f.exec(t, `cat warp.log | awk 'NAMESPACE == "callbox"'`, f.full()).Rows
	for _, r := range rows {
		if !strings.Contains(r.Raw, "callbox") {
			t.Errorf("expected raw to contain 'callbox', got %q", r.Raw)
		}
	}
	// callbox in shard 0: lines 3, 5, 6, 10, 13, 15; in shard 1: 18, 20, 24. Total 9.
	if len(rows) != 9 {
		t.Errorf("expected 9 callbox rows, got %d", len(rows))
	}
}

func queryOrderedByTimestamp(t *testing.T, f *queryFixture) {
	rows := f.exec(t, `cat warp.log`, f.full()).Rows
	// Each Raw starts with an ISO-8601 timestamp prefix ("2026-03-28T10:00:01
	// ..."), so consecutive rows must have non-decreasing prefixes.
	prefix := func(raw string) string {
		if len(raw) >= 19 {
			return raw[:19]
		}
		return raw
	}
	for i := 1; i < len(rows); i++ {
		if prefix(rows[i].Raw) < prefix(rows[i-1].Raw) {
			t.Errorf("results not ordered: row %d (%q) before row %d (%q)", i-1, rows[i-1].Raw, i, rows[i].Raw)
		}
	}
}

func queryMergesAcrossShards(t *testing.T, f *queryFixture) {
	rows := f.exec(t, `cat warp.log | grep timeout`, f.full()).Rows
	// Shard-0 rows are at 10:xx (window t0..t1), shard-1 at 11:xx (t1..t2).
	var shard0, shard1 bool
	for _, r := range rows {
		switch {
		case strings.Contains(r.Raw, "T10:"):
			shard0 = true
		case strings.Contains(r.Raw, "T11:"):
			shard1 = true
		}
	}
	if !shard0 || !shard1 {
		t.Errorf("expected results from both shards, shard0=%v shard1=%v", shard0, shard1)
	}
}

func querySort(t *testing.T, f *queryFixture) {
	d := f.exec(t, `cat vimgym.log | awk 'LEVEL == "ERROR"' | sort`, f.full())
	if d.Text == "" {
		t.Fatal("expected text output from compute pipeline, got empty")
	}
	lines := nonEmptyLines(d.Text)
	for i := 1; i < len(lines); i++ {
		if lines[i] < lines[i-1] {
			t.Errorf("not sorted: line %d %q < line %d %q", i, lines[i], i-1, lines[i-1])
		}
	}
	// 5 ERROR lines from vimgym, as in the pushed-down test.
	if len(lines) != 5 {
		t.Errorf("expected 5 lines, got %d: %q", len(lines), lines)
	}
}

func queryWcCount(t *testing.T, f *queryFixture) {
	d := f.exec(t, `cat warp.log | grep timeout | wc -l`, f.full())
	if got := strings.TrimSpace(d.Text); got != "5" {
		t.Errorf("expected wc -l = 5, got %q", got)
	}
}

func querySortUniqCount(t *testing.T, f *queryFixture) {
	d := f.exec(t, `cat warp.log | awk 'LEVEL == "ERROR"' | sort | uniq -c`, f.full())
	lines := nonEmptyLines(d.Text)
	if len(lines) == 0 {
		t.Fatal("expected non-empty output from sort | uniq -c")
	}
	for _, l := range lines {
		// uniq -c output looks like "      1 2026-03-28T10:00:01 ERROR ..."
		trimmed := strings.TrimSpace(l)
		if len(trimmed) == 0 || trimmed[0] < '0' || trimmed[0] > '9' {
			t.Errorf("unexpected uniq -c output format: %q", l)
		}
	}
}

func queryAwkFieldExtract(t *testing.T, f *queryFixture) {
	// A simple $N print is fully pushed down as a projection: no compute stage,
	// so the result is rows.
	d := f.exec(t, `cat warp.log | grep ERROR | awk '{ print $3 }'`, f.full())
	if len(d.Rows) == 0 {
		t.Fatal("expected rows, got none")
	}
	for _, row := range d.Rows {
		if row.Raw != "vimgym" && row.Raw != "callbox" {
			t.Errorf("expected app name, got %q", row.Raw)
		}
	}
}

// The request to compute carries the field form of a stage that reads only some
// fields, beside the plain sql every compute and shard understands.
func querySendsFieldForm(t *testing.T, f *queryFixture) {
	log := &requestLog{inner: f.c.Compute}
	f.c.Compute = log
	f.exec(t, `cat vimgym.log | awk '{ n += $2 } END { print n }'`, f.full())
	if len(log.reqs) == 0 {
		t.Fatal("no request reached compute")
	}
	req := log.reqs[len(log.reqs)-1]
	if req.FieldSql == "" || req.FieldSql == req.Sql {
		t.Errorf("field sql %q beside sql %q", req.FieldSql, req.Sql)
	}
	if got := req.FieldSpec.GetPositions(); len(got) != 1 || got[0] != 2 || req.FieldSpec.Last || req.FieldSpec.Nf {
		t.Errorf("field spec %v, want just $2", req.FieldSpec)
	}
}

// Every query, a zero-stage shard-only one included, goes through compute.
func queryNoStagesThroughCompute(t *testing.T, f *queryFixture) {
	log := f.logRequests()
	if d := f.exec(t, `cat vimgym.log`, f.full()); len(d.Rows) == 0 && d.Text == "" {
		t.Fatal("query produced no output")
	}
	if len(log.reqs) != 1 {
		t.Fatalf("want exactly one ExecuteQuery call, got %d", len(log.reqs))
	}
}

// The trace ID on the context given to Execute reaches the compute call, which
// couriers it over gRPC, on the interactive path and the cron path both.
func queryTraceIDReachesCompute(t *testing.T, f *queryFixture) {
	log := f.logRequests()
	ctx := weftsdk.ContextWithTraceID(context.Background(), "trace-coord-1")
	res, err := f.c.Execute(ctx, `cat vimgym.log`, f.full(), f.s.PrincipalOf(""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := weftsdk.TraceIDFromContext(log.ctx); got != "trace-coord-1" {
		t.Fatalf("Execute: compute call saw trace %q, want trace-coord-1", got)
	}

	f.s.saveCron(t, "trace-watch", "every 1m", "cat /log/vimgym.log")
	w := f.s.watch(t, "trace-watch")
	ctx = weftsdk.ContextWithTraceID(context.Background(), "trace-coord-2")
	res, err = f.c.ExecuteForWatch(ctx, w.Pipeline, f.full(), &w)
	if err != nil {
		t.Fatalf("ExecuteForWatch: %v", err)
	}
	if _, err := res.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := weftsdk.TraceIDFromContext(log.ctx); got != "trace-coord-2" {
		t.Fatalf("ExecuteForWatch: compute call saw trace %q, want trace-coord-2", got)
	}
}
