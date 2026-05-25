package coordinator

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kevlarcon/weft/compute"
	"github.com/kevlarcon/weft/compute/computetest"
	"github.com/kevlarcon/weft/logdb"
	"github.com/kevlarcon/weft/planner"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
	"gorm.io/gorm/clause"
)

// poolFromCoord returns the *compute.LocalComputePool that NewCoordinator
// installs by default. Fails the test if some earlier code wrapped or
// replaced c.Pool — that's a setup-order bug.
func poolFromCoord(t testing.TB, c *Coordinator) *compute.LocalComputePool {
	t.Helper()
	pool, ok := c.Pool.(*compute.LocalComputePool)
	if !ok {
		t.Fatalf("expected c.Pool to be *compute.LocalComputePool, got %T", c.Pool)
	}
	return pool
}

// addInProcessShard wires e.Shard into both c.Provisioner (so query
// planning sees the entry) and c.Pool's resolver overrides (so
// compute can run QueryToSink against the in-process handle without
// dialing). This is the unit-test replacement for the old
// registryPool bypass — compute still owns the dispatch, but the
// resolver tells it "for this id, use this handle directly."
func addInProcessShard(t testing.TB, c *Coordinator, e *logdb.ShardEntry, apps ...string) {
	t.Helper()
	if e.Shard == nil {
		t.Fatal("addInProcessShard: entry must have non-nil Shard")
	}
	poolFromCoord(t, c).RegisterResolver(computetest.NewInProcessResolver(e.ID, e.Shard))
	logdb.SeedShard(c.Provisioner, e)
	stampShardServicesFromApps(t, c, e, apps)
}

// stampShardServicesFromApps wires a directly-seeded shard into the
// shard_services table the service-level read router resolves through
// (ShardDescriptorsFor → ShardIDsServingService). Production populates
// shard_services at christen (StampShardServices) and on live app-add;
// SeedShard skips that whole path, so this is the fixture stand-in.
//
// shard_services FKs to shard_records, and these in-process fixtures
// keep the registry store-less (so SeedShard never persists the row), so
// we first upsert the shard_records parent — every production shard has
// one — then stamp one row per (namespace, service) the shard serves.
//
// The services come from the apps list the call site passes (each a
// composite "ns/svc/kind" or bare "ns"), split into (namespace, service)
// the way the planner's pseudofile parser does. Each resolved pair is
// both written to shard_services (read routing) and appended to
// e.Services (the in-memory write-routing membership christen would set).
//
// Apps whose namespace isn't registered under the store's sole test
// customer are skipped: a query for them resolves namespace_id 0 (no
// filter) and still reaches every shard, so the shard stays visible.
func stampShardServicesFromApps(t testing.TB, c *Coordinator, e *logdb.ShardEntry, apps []string) {
	t.Helper()
	if c.Store == nil {
		return
	}
	// These fixtures create exactly one customer (testQueryStore takes a
	// single name). Resolve it so namespace lookups are customer-scoped
	// without threading the name through every call site.
	var custs []store.Customer
	if err := c.Store.DB().Find(&custs).Error; err != nil {
		t.Fatalf("stampShardServicesFromApps: list customers: %v", err)
	}
	if len(custs) != 1 {
		return // ambiguous / no customer — leave routing to the nsID==0 path
	}
	custID := custs[0].ID

	// Upsert the shard_records parent so shard_services' FK holds. Mirror
	// the entry's fields; default a zero BucketID to the default bucket
	// (NOT NULL FK). RFC3339 windows match the registry's own persist().
	bucketID := e.BucketID
	if bucketID == 0 {
		def, ok, err := c.Store.GetBucketByLabel("default")
		if err != nil || !ok {
			t.Fatalf("stampShardServicesFromApps: default bucket: ok=%v err=%v", ok, err)
		}
		bucketID = def.ID
	}
	if err := c.Store.SaveShard(&store.ShardRecord{
		ID:          e.ID,
		WindowStart: e.WindowStart.Format(time.RFC3339),
		WindowEnd:   e.WindowEnd.Format(time.RFC3339),
		State:       string(e.State),
		Tier:        e.Tier,
		BucketID:    bucketID,
		CreatedAt:   e.CreatedAt.Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("stampShardServicesFromApps: persist shard_records %s: %v", e.ID, err)
	}

	seen := map[string]struct{}{}
	for _, app := range apps {
		ns, svc := app, ""
		if i := strings.IndexByte(app, '/'); i >= 0 {
			ns = app[:i]
			rest := app[i+1:]
			svc = rest
			if j := strings.IndexByte(rest, '/'); j >= 0 {
				svc = rest[:j]
			}
		}
		n, err := c.Store.GetNamespace(custID, ns)
		if err != nil {
			continue // namespace not registered in this fixture — skip
		}
		key := fmt.Sprintf("%d/%s", n.ID, svc)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if err := c.Store.DB().Clauses(clause.OnConflict{DoNothing: true}).Create(&store.ShardService{
			ShardID: e.ID, NamespaceID: n.ID, Service: svc,
		}).Error; err != nil {
			t.Fatalf("stampShardServicesFromApps: stamp %s (%s/%s): %v", e.ID, ns, svc, err)
		}
		e.Services = append(e.Services, store.ShardService{
			ShardID: e.ID, NamespaceID: n.ID, Service: svc,
		})
	}
}

// Base times for test fixtures. Shard 0 covers 10:00–11:00, shard 1 covers 11:00–12:00.
var (
	t0 = time.Date(2026, 3, 28, 10, 0, 0, 0, time.UTC) // shard 0 start
	t1 = time.Date(2026, 3, 28, 11, 0, 0, 0, time.UTC) // shard 1 start
	t2 = time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC) // shard 1 end
)

// fullRange covers both shards.
var fullRange = TimeRange{Start: t0, End: t2}

func payload(m map[string]any) []byte {
	b, _ := json.Marshal(m)
	return b
}

// setupTestCoordinator creates two in-memory DuckDB shards with realistic
// log data and returns a Coordinator ready for querying. Takes
// testing.TB so benchmarks can reuse the fixture without duplicating
// the shard/registry boilerplate.
func setupTestCoordinator(t testing.TB) *Coordinator {
	t.Helper()

	s0, err := shard.NewDuckDBShard("shard-0", "")
	if err != nil {
		t.Fatalf("create shard-0: %v", err)
	}
	t.Cleanup(func() { s0.Close() })

	s1, err := shard.NewDuckDBShard("shard-1", "")
	if err != nil {
		t.Fatalf("create shard-1: %v", err)
	}
	t.Cleanup(func() { s1.Close() })

	// --- Shard 0: 10:00–11:00, 25 rows ---
	if err := s0.Ingest(shard0Rows(), nil); err != nil {
		t.Fatalf("ingest shard-0: %v", err)
	}

	// --- Shard 1: 11:00–12:00, 25 rows ---
	if err := s1.Ingest(shard1Rows(), nil); err != nil {
		t.Fatalf("ingest shard-1: %v", err)
	}

	// Inject some rows from a different customer to test isolation.
	if err := s0.Ingest(otherCustomerRows(), nil); err != nil {
		t.Fatalf("ingest other-customer: %v", err)
	}

	apps := []string{"vimgym", "callbox"}
	c := NewCoordinator(CoordinatorConfig{Store: testQueryStore(t, "kevlar", "vimgym", "callbox", "warp")})
	addInProcessShard(t, c, &logdb.ShardEntry{
		ID:          "shard-0",
		WindowStart: t0, WindowEnd: t1,
		State: logdb.StateIngesting, Shard: s0,
		CreatedAt: time.Now(),
	}, apps...)
	addInProcessShard(t, c, &logdb.ShardEntry{
		ID:          "shard-1",
		WindowStart: t1, WindowEnd: t2,
		State: logdb.StateIngesting, Shard: s1,
		CreatedAt: time.Now(),
	}, apps...)
	return c
}

func shard0Rows() []*pb.LogRow {
	base := t0
	return []*pb.LogRow{
		{Raw: "2026-03-28T10:00:01 ERROR vimgym i-001 connection timeout", Fields: []string{"2026-03-28T10:00:01", "ERROR", "vimgym", "i-001", "connection", "timeout"}, TsUnixNs: base.Add(1 * time.Second).UnixNano(), Level: "ERROR", Namespace: "vimgym", Instance: "i-001", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 500, "action": "login", "user_id": "u1"})},
		{Raw: "2026-03-28T10:00:05 INFO vimgym i-001 request completed", Fields: []string{"2026-03-28T10:00:05", "INFO", "vimgym", "i-001", "request", "completed"}, TsUnixNs: base.Add(5 * time.Second).UnixNano(), Level: "INFO", Namespace: "vimgym", Instance: "i-001", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 200, "action": "list", "duration_ms": 45})},
		{Raw: "2026-03-28T10:01:00 WARN callbox i-002 slow query detected", Fields: []string{"2026-03-28T10:01:00", "WARN", "callbox", "i-002", "slow", "query", "detected"}, TsUnixNs: base.Add(1 * time.Minute).UnixNano(), Level: "WARN", Namespace: "callbox", Instance: "i-002", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 200, "action": "search", "duration_ms": 1500})},
		{Raw: "2026-03-28T10:02:00 ERROR vimgym i-001 null pointer exception", Fields: []string{"2026-03-28T10:02:00", "ERROR", "vimgym", "i-001", "null", "pointer", "exception"}, TsUnixNs: base.Add(2 * time.Minute).UnixNano(), Level: "ERROR", Namespace: "vimgym", Instance: "i-001", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 500, "action": "update", "user_id": "u2"})},
		{Raw: "2026-03-28T10:03:00 INFO callbox i-002 health check ok", Fields: []string{"2026-03-28T10:03:00", "INFO", "callbox", "i-002", "health", "check", "ok"}, TsUnixNs: base.Add(3 * time.Minute).UnixNano(), Level: "INFO", Namespace: "callbox", Instance: "i-002", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 200, "action": "health"})},
		{Raw: "2026-03-28T10:05:00 ERROR callbox i-002 connection timeout to database", Fields: []string{"2026-03-28T10:05:00", "ERROR", "callbox", "i-002", "connection", "timeout", "to", "database"}, TsUnixNs: base.Add(5 * time.Minute).UnixNano(), Level: "ERROR", Namespace: "callbox", Instance: "i-002", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 503, "action": "query"})},
		{Raw: "2026-03-28T10:10:00 INFO vimgym i-003 user signup completed", Fields: []string{"2026-03-28T10:10:00", "INFO", "vimgym", "i-003", "user", "signup", "completed"}, TsUnixNs: base.Add(10 * time.Minute).UnixNano(), Level: "INFO", Namespace: "vimgym", Instance: "i-003", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 201, "action": "signup", "user_id": "u3"})},
		{Raw: "2026-03-28T10:15:00 DEBUG vimgym i-001 cache miss for key session:u1", Fields: []string{"2026-03-28T10:15:00", "DEBUG", "vimgym", "i-001", "cache", "miss", "for", "key", "session:u1"}, TsUnixNs: base.Add(15 * time.Minute).UnixNano(), Level: "DEBUG", Namespace: "vimgym", Instance: "i-001", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 200, "action": "cache_lookup"})},
		{Raw: "2026-03-28T10:20:00 ERROR vimgym i-003 timeout waiting for response", Fields: []string{"2026-03-28T10:20:00", "ERROR", "vimgym", "i-003", "timeout", "waiting", "for", "response"}, TsUnixNs: base.Add(20 * time.Minute).UnixNano(), Level: "ERROR", Namespace: "vimgym", Instance: "i-003", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 504, "action": "proxy"})},
		{Raw: "2026-03-28T10:25:00 INFO callbox i-002 batch job completed 150 records", Fields: []string{"2026-03-28T10:25:00", "INFO", "callbox", "i-002", "batch", "job", "completed", "150", "records"}, TsUnixNs: base.Add(25 * time.Minute).UnixNano(), Level: "INFO", Namespace: "callbox", Instance: "i-002", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 200, "action": "batch", "count": 150})},
		{Raw: "2026-03-28T10:30:00 WARN vimgym i-001 high memory usage 85%", Fields: []string{"2026-03-28T10:30:00", "WARN", "vimgym", "i-001", "high", "memory", "usage", "85%"}, TsUnixNs: base.Add(30 * time.Minute).UnixNano(), Level: "WARN", Namespace: "vimgym", Instance: "i-001", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 200, "action": "monitor", "memory_pct": 85})},
		{Raw: "2026-03-28T10:35:00 INFO vimgym i-001 request completed", Fields: []string{"2026-03-28T10:35:00", "INFO", "vimgym", "i-001", "request", "completed"}, TsUnixNs: base.Add(35 * time.Minute).UnixNano(), Level: "INFO", Namespace: "vimgym", Instance: "i-001", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 200, "action": "list", "duration_ms": 32})},
		{Raw: "2026-03-28T10:40:00 ERROR callbox i-002 authentication failed for tenant acme", Fields: []string{"2026-03-28T10:40:00", "ERROR", "callbox", "i-002", "authentication", "failed", "for", "tenant", "acme"}, TsUnixNs: base.Add(40 * time.Minute).UnixNano(), Level: "ERROR", Namespace: "callbox", Instance: "i-002", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 401, "action": "auth"})},
		{Raw: "2026-03-28T10:45:00 INFO vimgym i-003 deploy v2.3.1 started", Fields: []string{"2026-03-28T10:45:00", "INFO", "vimgym", "i-003", "deploy", "v2.3.1", "started"}, TsUnixNs: base.Add(45 * time.Minute).UnixNano(), Level: "INFO", Namespace: "vimgym", Instance: "i-003", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 200, "action": "deploy", "version": "v2.3.1"})},
		{Raw: "2026-03-28T10:50:00 INFO callbox i-002 connection pool resized to 20", Fields: []string{"2026-03-28T10:50:00", "INFO", "callbox", "i-002", "connection", "pool", "resized", "to", "20"}, TsUnixNs: base.Add(50 * time.Minute).UnixNano(), Level: "INFO", Namespace: "callbox", Instance: "i-002", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 200, "action": "pool_resize", "size": 20})},
	}
}

func shard1Rows() []*pb.LogRow {
	base := t1
	return []*pb.LogRow{
		{Raw: "2026-03-28T11:00:01 INFO vimgym i-001 deploy v2.3.1 completed", Fields: []string{"2026-03-28T11:00:01", "INFO", "vimgym", "i-001", "deploy", "v2.3.1", "completed"}, TsUnixNs: base.Add(1 * time.Second).UnixNano(), Level: "INFO", Namespace: "vimgym", Instance: "i-001", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 200, "action": "deploy", "version": "v2.3.1"})},
		{Raw: "2026-03-28T11:01:00 ERROR vimgym i-001 500 internal server error on /api/users", Fields: []string{"2026-03-28T11:01:00", "ERROR", "vimgym", "i-001", "500", "internal", "server", "error", "on", "/api/users"}, TsUnixNs: base.Add(1 * time.Minute).UnixNano(), Level: "ERROR", Namespace: "vimgym", Instance: "i-001", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 500, "action": "api", "path": "/api/users"})},
		{Raw: "2026-03-28T11:05:00 WARN callbox i-002 disk usage above 80%", Fields: []string{"2026-03-28T11:05:00", "WARN", "callbox", "i-002", "disk", "usage", "above", "80%"}, TsUnixNs: base.Add(5 * time.Minute).UnixNano(), Level: "WARN", Namespace: "callbox", Instance: "i-002", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 200, "action": "monitor", "disk_pct": 82})},
		{Raw: "2026-03-28T11:10:00 INFO vimgym i-003 cache warmed 2500 keys", Fields: []string{"2026-03-28T11:10:00", "INFO", "vimgym", "i-003", "cache", "warmed", "2500", "keys"}, TsUnixNs: base.Add(10 * time.Minute).UnixNano(), Level: "INFO", Namespace: "vimgym", Instance: "i-003", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 200, "action": "cache_warm", "keys": 2500})},
		{Raw: "2026-03-28T11:15:00 ERROR callbox i-002 timeout connecting to redis", Fields: []string{"2026-03-28T11:15:00", "ERROR", "callbox", "i-002", "timeout", "connecting", "to", "redis"}, TsUnixNs: base.Add(15 * time.Minute).UnixNano(), Level: "ERROR", Namespace: "callbox", Instance: "i-002", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 503, "action": "cache"})},
		{Raw: "2026-03-28T11:20:00 INFO vimgym i-001 request completed", Fields: []string{"2026-03-28T11:20:00", "INFO", "vimgym", "i-001", "request", "completed"}, TsUnixNs: base.Add(20 * time.Minute).UnixNano(), Level: "INFO", Namespace: "vimgym", Instance: "i-001", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 200, "action": "list", "duration_ms": 28})},
		{Raw: "2026-03-28T11:25:00 WARN vimgym i-003 request latency spike p99=2.3s", Fields: []string{"2026-03-28T11:25:00", "WARN", "vimgym", "i-003", "request", "latency", "spike", "p99=2.3s"}, TsUnixNs: base.Add(25 * time.Minute).UnixNano(), Level: "WARN", Namespace: "vimgym", Instance: "i-003", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 200, "action": "monitor", "p99_ms": 2300})},
		{Raw: "2026-03-28T11:30:00 ERROR vimgym i-001 connection timeout to upstream", Fields: []string{"2026-03-28T11:30:00", "ERROR", "vimgym", "i-001", "connection", "timeout", "to", "upstream"}, TsUnixNs: base.Add(30 * time.Minute).UnixNano(), Level: "ERROR", Namespace: "vimgym", Instance: "i-001", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 504, "action": "proxy"})},
		{Raw: "2026-03-28T11:40:00 INFO callbox i-002 health check ok", Fields: []string{"2026-03-28T11:40:00", "INFO", "callbox", "i-002", "health", "check", "ok"}, TsUnixNs: base.Add(40 * time.Minute).UnixNano(), Level: "INFO", Namespace: "callbox", Instance: "i-002", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 200, "action": "health"})},
		{Raw: "2026-03-28T11:50:00 INFO vimgym i-003 graceful shutdown initiated", Fields: []string{"2026-03-28T11:50:00", "INFO", "vimgym", "i-003", "graceful", "shutdown", "initiated"}, TsUnixNs: base.Add(50 * time.Minute).UnixNano(), Level: "INFO", Namespace: "vimgym", Instance: "i-003", Customer: "kevlar", PayloadJson: payload(map[string]any{"status": 200, "action": "shutdown"})},
	}
}

// otherCustomerRows returns rows from a different customer to test isolation.
func otherCustomerRows() []*pb.LogRow {
	return []*pb.LogRow{
		{Raw: "2026-03-28T10:00:30 ERROR vimgym i-999 should not appear", Fields: []string{"2026-03-28T10:00:30", "ERROR", "vimgym", "i-999"}, TsUnixNs: t0.Add(30 * time.Second).UnixNano(), Level: "ERROR", Namespace: "vimgym", Instance: "i-999", Customer: "acme", PayloadJson: payload(map[string]any{"status": 500})},
		{Raw: "2026-03-28T10:01:30 INFO callbox i-999 should not appear", Fields: []string{"2026-03-28T10:01:30", "INFO", "callbox", "i-999"}, TsUnixNs: t0.Add(90 * time.Second).UnixNano(), Level: "INFO", Namespace: "callbox", Instance: "i-999", Customer: "acme", PayloadJson: payload(map[string]any{"status": 200})},
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestQuery_AwkLevelFilter(t *testing.T) {
	c := setupTestCoordinator(t)
	res, err := c.Execute(`cat vimgym.log | awk '$LEVEL == "ERROR"'`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	rows := resD.Rows
	for _, r := range rows {
		if !strings.Contains(r.Raw, "vimgym") {
			t.Errorf("expected raw to contain 'vimgym', got %q", r.Raw)
		}
		if !strings.Contains(r.Raw, "ERROR") {
			t.Errorf("expected raw to contain 'ERROR', got %q", r.Raw)
		}
	}
	// Shard 0 has vimgym ERROR lines: 1, 4, 9. Shard 1 has: 17, 23. Total 5.
	if len(rows) != 5 {
		t.Errorf("expected 5 rows, got %d", len(rows))
	}
}

func TestQuery_GrepTimeout(t *testing.T) {
	c := setupTestCoordinator(t)
	res, err := c.Execute(`cat warp.log | grep timeout`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	rows := resD.Rows
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

func TestQuery_JqSelectStatus500(t *testing.T) {
	c := setupTestCoordinator(t)
	res, err := c.Execute(`cat vimgym.log | jq 'select(.status == 500)'`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	rows := resD.Rows
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

func TestQuery_HeadLimit(t *testing.T) {
	c := setupTestCoordinator(t)
	res, err := c.Execute(`head -3 vimgym.log`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	rows := resD.Rows
	if len(rows) > 3 {
		t.Errorf("expected at most 3 rows, got %d", len(rows))
	}
	for _, r := range rows {
		if !strings.Contains(r.Raw, "vimgym") {
			t.Errorf("expected raw to contain 'vimgym', got %q", r.Raw)
		}
	}
}

func TestQuery_TailLimit(t *testing.T) {
	c := setupTestCoordinator(t)
	res, err := c.Execute(`tail -3 vimgym.log`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	rows := resD.Rows
	if len(rows) > 3 {
		t.Errorf("expected at most 3 rows, got %d", len(rows))
	}
	for _, r := range rows {
		if !strings.Contains(r.Raw, "vimgym") {
			t.Errorf("expected raw to contain 'vimgym', got %q", r.Raw)
		}
	}
}

func TestQuery_TimeRangeScoping_Shard0Only(t *testing.T) {
	c := setupTestCoordinator(t)
	tr := TimeRange{Start: t0, End: t1} // only shard 0
	res, err := c.Execute(`cat vimgym.log | awk '$LEVEL == "ERROR"'`, tr, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	rows := resD.Rows
	// Shard 0 vimgym ERROR: lines 1, 4, 9 — all timestamped 10:xx.
	if len(rows) != 3 {
		t.Errorf("expected 3 rows from shard 0, got %d", len(rows))
	}
	// Shard-0 rows have timestamps 10:xx (window t0..t1); shard-1
	// rows would be 11:xx (window t1..t2). Check the timestamp prefix
	// in Raw — warp's bytes-only stdout drops
	// metadata, but Raw still carries the original timestamp.
	for _, r := range rows {
		if !strings.Contains(r.Raw, "T10:") {
			t.Errorf("got row outside shard 0 range: %q", r.Raw)
		}
	}
}

func TestQuery_TimeRangeScoping_Shard1Only(t *testing.T) {
	c := setupTestCoordinator(t)
	tr := TimeRange{Start: t1, End: t2} // only shard 1
	res, err := c.Execute(`cat vimgym.log | awk '$LEVEL == "ERROR"'`, tr, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	rows := resD.Rows
	// Shard 1 vimgym ERROR: lines 17, 23 — both timestamped 11:xx.
	if len(rows) != 2 {
		t.Errorf("expected 2 rows from shard 1, got %d", len(rows))
	}
	// Shard-1 rows are 11:xx; reject anything from shard 0 (10:xx).
	for _, r := range rows {
		if !strings.Contains(r.Raw, "T11:") {
			t.Errorf("got row outside shard 1 range: %q", r.Raw)
		}
	}
}

func TestQuery_CustomerIsolation(t *testing.T) {
	c := setupTestCoordinator(t)
	res, err := c.Execute(`cat vimgym.log`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	rows := resD.Rows
	// Customer isolation is enforced at the SQL level (WHERE customer = 'kevlar').
	// With ResultRow we can only verify none of the "acme" rows leaked through.
	for _, r := range rows {
		if strings.Contains(r.Raw, "should not appear") {
			t.Fatal("customer isolation broken: acme row leaked through")
		}
	}
}

func TestQuery_WarpLogAppFilter(t *testing.T) {
	c := setupTestCoordinator(t)
	res, err := c.Execute(`cat warp.log | awk '$NAMESPACE == "callbox"'`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	rows := resD.Rows
	for _, r := range rows {
		if !strings.Contains(r.Raw, "callbox") {
			t.Errorf("expected raw to contain 'callbox', got %q", r.Raw)
		}
	}
	// Callbox rows in kevlar: shard 0 has 5 (lines 3,5,6,10,13,15=6), shard 1 has 3 (18,20,24)
	// Let me count: shard0 callbox = 3,5,6,10,13,15 = 6; shard1 callbox = 18,20,24 = 3; total 9
	if len(rows) != 9 {
		t.Errorf("expected 9 callbox rows, got %d", len(rows))
		for _, r := range rows {
			t.Logf("  %s", r.Raw)
		}
	}
}

func TestQuery_ResultsOrderedByTimestamp(t *testing.T) {
	c := setupTestCoordinator(t)
	res, err := c.Execute(`cat warp.log`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	rows := resD.Rows
	// Order: each Raw starts with an ISO-8601 timestamp prefix
	// ("2026-03-28T10:00:01 ..."). Sort-stable ordering through the
	// pipeline means consecutive rows must have non-decreasing
	// timestamp prefixes. (Pre-streaming-runLocal we asserted via
	// metadata since warp's bytes-only stdout drops it; the
	// timestamp on the wire is the surviving order key.)
	tsPrefix := func(raw string) string {
		// First 19 chars are "YYYY-MM-DDTHH:MM:SS" given the fixture.
		if len(raw) >= 19 {
			return raw[:19]
		}
		return raw
	}
	for i := 1; i < len(rows); i++ {
		if tsPrefix(rows[i].Raw) < tsPrefix(rows[i-1].Raw) {
			t.Errorf("results not ordered: row %d (%q) before row %d (%q)",
				i-1, rows[i-1].Raw, i, rows[i].Raw)
		}
	}
}

func TestQuery_MergesAcrossShards(t *testing.T) {
	c := setupTestCoordinator(t)
	res, err := c.Execute(`cat warp.log | grep timeout`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	rows := resD.Rows
	// Detect shard origin via the timestamp prefix in Raw: shard-0
	// rows are at 10:xx (window t0..t1), shard-1 at 11:xx (t1..t2).
	// the unified-dispatch path
	// renders rows through warp's stdout, which is bytes-only —
	// Raw still carries the original timestamp.
	hasShard0 := false
	hasShard1 := false
	for _, r := range rows {
		switch {
		case strings.Contains(r.Raw, "T10:"):
			hasShard0 = true
		case strings.Contains(r.Raw, "T11:"):
			hasShard1 = true
		}
	}
	if !hasShard0 || !hasShard1 {
		t.Errorf("expected results from both shards, shard0=%v shard1=%v", hasShard0, hasShard1)
	}
}

// ---------------------------------------------------------------------------
// Compute stage tests
// ---------------------------------------------------------------------------

func setupTestCoordinatorWithCompute(t *testing.T) *Coordinator {
	t.Helper()
	return setupTestCoordinator(t)
}

func TestCompute_Sort(t *testing.T) {
	c := setupTestCoordinatorWithCompute(t)
	res, err := c.Execute(`cat vimgym.log | awk '$LEVEL == "ERROR"' | sort`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	if resD.Text == "" {
		t.Fatal("expected text output from compute pipeline, got empty")
	}
	lines := nonEmptyLines(resD.Text)
	// Should be sorted lexicographically.
	for i := 1; i < len(lines); i++ {
		if lines[i] < lines[i-1] {
			t.Errorf("not sorted: line %d %q < line %d %q", i, lines[i], i-1, lines[i-1])
		}
	}
	// Should have 5 ERROR lines from vimgym (same count as non-compute test).
	if len(lines) != 5 {
		t.Errorf("expected 5 lines, got %d", len(lines))
		for _, l := range lines {
			t.Logf("  %s", l)
		}
	}
}

func TestCompute_WcCount(t *testing.T) {
	c := setupTestCoordinatorWithCompute(t)
	res, err := c.Execute(`cat warp.log | grep timeout | wc -l`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	if resD.Text == "" {
		t.Fatal("expected text output, got empty")
	}
	// wc -l should output the count of timeout lines.
	// We know there are 5 timeout lines from the grep test.
	trimmed := strings.TrimSpace(resD.Text)
	if trimmed != "5" {
		t.Errorf("expected wc -l = 5, got %q", trimmed)
	}
}

func TestCompute_SortUniqCount(t *testing.T) {
	c := setupTestCoordinatorWithCompute(t)
	// Get ERROR lines from all apps, sort them, count unique lines.
	res, err := c.Execute(`cat warp.log | awk '$LEVEL == "ERROR"' | sort | uniq -c`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	if resD.Text == "" {
		t.Fatal("expected text output, got empty")
	}
	lines := nonEmptyLines(resD.Text)
	// Each unique ERROR line should appear with a count prefix.
	if len(lines) == 0 {
		t.Error("expected non-empty output from sort | uniq -c")
	}
	for _, l := range lines {
		// uniq -c output looks like "      1 2026-03-28T10:00:01 ERROR ..."
		trimmed := strings.TrimSpace(l)
		if len(trimmed) == 0 || trimmed[0] < '0' || trimmed[0] > '9' {
			t.Errorf("unexpected uniq -c output format: %q", l)
		}
	}
}

func TestCompute_AwkFieldExtract(t *testing.T) {
	c := setupTestCoordinatorWithCompute(t)
	// Grep for ERROR, then print just the third field (app name).
	// Simple $N print is fully pushed down as a projection — no compute stage.
	res, err := c.Execute(`cat warp.log | grep ERROR | awk '{ print $3 }'`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	// Projection returns rows (not text) since there are no compute stages.
	if len(resD.Rows) == 0 {
		t.Fatal("expected rows, got none")
	}
	for _, row := range resD.Rows {
		if row.Raw != "vimgym" && row.Raw != "callbox" {
			t.Errorf("expected app name, got %q", row.Raw)
		}
	}
}

// TestExecuteQuery_DescriptorFields verifies that LambdaFunctionURL
// from CoordinatorConfig propagates into the ExecuteQueryRequest that
// reaches the compute pool, and that live/archived shard descriptors
// are populated correctly. SHARD_SECRET no longer travels through
// the request — compute reads it from its own env.
func TestExecuteQuery_DescriptorFields(t *testing.T) {
	s0, err := shard.NewDuckDBShard("shard-live", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s0.Close() })
	if err := s0.Ingest(shard0Rows(), nil); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	apps := []string{"vimgym", "callbox"}

	c := NewCoordinator(CoordinatorConfig{
		LambdaFunctionURL: "https://lambda.example.com/cold-query",
	})
	// Register in-process resolvers on the default LocalComputePool
	// BEFORE wrapping in spyPool, because addInProcessShard reaches
	// for the underlying *compute.LocalComputePool through c.Pool.
	addInProcessShard(t, c, &logdb.ShardEntry{
		ID:          "shard-live",
		WindowStart: now.Add(-2 * time.Hour), WindowEnd: now.Add(-1 * time.Hour),
		State: logdb.StateIngesting, Shard: s0, Addr: "10.0.0.1:9000",
		CreatedAt: now,
	}, apps...)
	addInProcessShard(t, c, &logdb.ShardEntry{
		ID:          "shard-archived",
		WindowStart: now.Add(-1 * time.Hour), WindowEnd: now,
		State: logdb.StateArchived, Shard: s0, ParquetPath: "s3://bucket/shard-archived.parquet",
		RowCount:  1,
		CreatedAt: now,
	}, apps...)
	spy := &spyPool{inner: c.Pool}
	c.Pool = spy

	tr := TimeRange{Start: now.Add(-2 * time.Hour), End: now}
	plan := &planner.QueryPlan{
		ShardSQL:       "SELECT raw, ts FROM logs WHERE customer = 'kevlar' ORDER BY ts",
		DirectShardSQL: "SELECT raw, ts FROM logs WHERE customer = 'kevlar' ORDER BY ts",
	}

	_, err = c.executeQuery(plan, tr, "kevlar", 0, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if spy.lastReq == nil {
		t.Fatal("pool never received an ExecuteQueryRequest")
	}

	descByID := map[string]*pb.ShardDescriptor{}
	for _, d := range spy.lastReq.Shards {
		descByID[d.ShardId] = d
	}

	if live := descByID["shard-live"]; live == nil {
		t.Fatal("missing descriptor for shard-live")
	} else if live.GrpcAddr != "10.0.0.1:9000" {
		t.Errorf("shard-live GrpcAddr = %q, want %q", live.GrpcAddr, "10.0.0.1:9000")
	}

	if archived := descByID["shard-archived"]; archived == nil {
		t.Fatal("missing descriptor for shard-archived")
	} else {
		if archived.LambdaUrl != "https://lambda.example.com/cold-query" {
			t.Errorf("shard-archived LambdaUrl = %q, want %q", archived.LambdaUrl, "https://lambda.example.com/cold-query")
		}
		if archived.ParquetPath != "s3://bucket/shard-archived.parquet" {
			t.Errorf("shard-archived ParquetPath = %q, want %q", archived.ParquetPath, "s3://bucket/shard-archived.parquet")
		}
	}
}

// nonEmptyLines splits text into lines, filtering out empty ones.
func nonEmptyLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
