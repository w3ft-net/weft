package coordinator

import (
	"encoding/json"
	"time"

	"github.com/kevlarcon/weft/compute"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// DevFixtureTime constants for the sample data time windows.
var (
	DevT0 = time.Date(2026, 3, 28, 10, 0, 0, 0, time.UTC) // shard 0 start
	DevT1 = time.Date(2026, 3, 28, 11, 0, 0, 0, time.UTC) // shard 1 start
	DevT2 = time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC) // shard 1 end
)

func devPayload(m map[string]any) []byte {
	b, _ := json.Marshal(m)
	return b

}

func devRow(lineNum int64, raw string, fields []string, ts time.Time, level, app, instance, customer string, payload []byte) *pb.LogRow {
	return &pb.LogRow{
		LineNum:     lineNum,
		Raw:         raw,
		Fields:      fields,
		TsUnixNs:    ts.UnixNano(),
		Level:       level,
		App:         app,
		Instance:    instance,
		Customer:    customer,
		PayloadJson: payload,
	}
}

// NewDevCoordinator creates a coordinator with two in-memory DuckDB shards
// populated with sample log data. Useful for development and demos.
// The caller must call the returned cleanup function when done.
func NewDevCoordinator() (*Coordinator, func(), error) {
	s0, err := shard.NewDuckDBShard("shard-0", "")
	if err != nil {
		return nil, nil, err
	}

	s1, err := shard.NewDuckDBShard("shard-1", "")
	if err != nil {
		s0.Close()
		return nil, nil, err
	}

	if err := s0.Ingest(devShard0Rows()); err != nil {
		s0.Close()
		s1.Close()
		return nil, nil, err
	}
	if err := s1.Ingest(devShard1Rows()); err != nil {
		s0.Close()
		s1.Close()
		return nil, nil, err
	}

	cleanup := func() {
		s0.Close()
		s1.Close()
	}

	c := NewCoordinator(CoordinatorConfig{
		Shards: []shard.Shard{s0, s1},
		Pool:   &compute.LocalComputePool{Parallelism: 2},
	})

	return c, cleanup, nil
}

// DevApps returns the list of app names available in the dev fixtures.
func DevApps() []string {
	return []string{"vimgym", "callbox"}
}

// DevShardRows returns fixture rows for the named shard ("shard-0" or "shard-1").
// Returns nil for unknown names.
func DevShardRows(name string) []*pb.LogRow {
	switch name {
	case "shard-0":
		return devShard0Rows()
	case "shard-1":
		return devShard1Rows()
	}
	return nil
}

func devShard0Rows() []*pb.LogRow {
	base := DevT0
	return []*pb.LogRow{
		devRow(1, "2026-03-28T10:00:01 ERROR vimgym i-001 connection timeout", []string{"2026-03-28T10:00:01", "ERROR", "vimgym", "i-001", "connection", "timeout"}, base.Add(1*time.Second), "ERROR", "vimgym", "i-001", "kevlar", devPayload(map[string]any{"status": 500, "action": "login", "user_id": "u1"})),
		devRow(2, "2026-03-28T10:00:05 INFO vimgym i-001 request completed", []string{"2026-03-28T10:00:05", "INFO", "vimgym", "i-001", "request", "completed"}, base.Add(5*time.Second), "INFO", "vimgym", "i-001", "kevlar", devPayload(map[string]any{"status": 200, "action": "list", "duration_ms": 45})),
		devRow(3, "2026-03-28T10:01:00 WARN callbox i-002 slow query detected", []string{"2026-03-28T10:01:00", "WARN", "callbox", "i-002", "slow", "query", "detected"}, base.Add(1*time.Minute), "WARN", "callbox", "i-002", "kevlar", devPayload(map[string]any{"status": 200, "action": "search", "duration_ms": 1500})),
		devRow(4, "2026-03-28T10:02:00 ERROR vimgym i-001 null pointer exception", []string{"2026-03-28T10:02:00", "ERROR", "vimgym", "i-001", "null", "pointer", "exception"}, base.Add(2*time.Minute), "ERROR", "vimgym", "i-001", "kevlar", devPayload(map[string]any{"status": 500, "action": "update", "user_id": "u2"})),
		devRow(5, "2026-03-28T10:03:00 INFO callbox i-002 health check ok", []string{"2026-03-28T10:03:00", "INFO", "callbox", "i-002", "health", "check", "ok"}, base.Add(3*time.Minute), "INFO", "callbox", "i-002", "kevlar", devPayload(map[string]any{"status": 200, "action": "health"})),
		devRow(6, "2026-03-28T10:05:00 ERROR callbox i-002 connection timeout to database", []string{"2026-03-28T10:05:00", "ERROR", "callbox", "i-002", "connection", "timeout", "to", "database"}, base.Add(5*time.Minute), "ERROR", "callbox", "i-002", "kevlar", devPayload(map[string]any{"status": 503, "action": "query"})),
		devRow(7, "2026-03-28T10:10:00 INFO vimgym i-003 user signup completed", []string{"2026-03-28T10:10:00", "INFO", "vimgym", "i-003", "user", "signup", "completed"}, base.Add(10*time.Minute), "INFO", "vimgym", "i-003", "kevlar", devPayload(map[string]any{"status": 201, "action": "signup", "user_id": "u3"})),
		devRow(8, "2026-03-28T10:15:00 DEBUG vimgym i-001 cache miss for key session:u1", []string{"2026-03-28T10:15:00", "DEBUG", "vimgym", "i-001", "cache", "miss", "for", "key", "session:u1"}, base.Add(15*time.Minute), "DEBUG", "vimgym", "i-001", "kevlar", devPayload(map[string]any{"status": 200, "action": "cache_lookup"})),
		devRow(9, "2026-03-28T10:20:00 ERROR vimgym i-003 timeout waiting for response", []string{"2026-03-28T10:20:00", "ERROR", "vimgym", "i-003", "timeout", "waiting", "for", "response"}, base.Add(20*time.Minute), "ERROR", "vimgym", "i-003", "kevlar", devPayload(map[string]any{"status": 504, "action": "proxy"})),
		devRow(10, "2026-03-28T10:25:00 INFO callbox i-002 batch job completed 150 records", []string{"2026-03-28T10:25:00", "INFO", "callbox", "i-002", "batch", "job", "completed", "150", "records"}, base.Add(25*time.Minute), "INFO", "callbox", "i-002", "kevlar", devPayload(map[string]any{"status": 200, "action": "batch", "count": 150})),
		devRow(11, "2026-03-28T10:30:00 WARN vimgym i-001 high memory usage 85%", []string{"2026-03-28T10:30:00", "WARN", "vimgym", "i-001", "high", "memory", "usage", "85%"}, base.Add(30*time.Minute), "WARN", "vimgym", "i-001", "kevlar", devPayload(map[string]any{"status": 200, "action": "monitor", "memory_pct": 85})),
		devRow(12, "2026-03-28T10:35:00 INFO vimgym i-001 request completed", []string{"2026-03-28T10:35:00", "INFO", "vimgym", "i-001", "request", "completed"}, base.Add(35*time.Minute), "INFO", "vimgym", "i-001", "kevlar", devPayload(map[string]any{"status": 200, "action": "list", "duration_ms": 32})),
		devRow(13, "2026-03-28T10:40:00 ERROR callbox i-002 authentication failed for tenant acme", []string{"2026-03-28T10:40:00", "ERROR", "callbox", "i-002", "authentication", "failed", "for", "tenant", "acme"}, base.Add(40*time.Minute), "ERROR", "callbox", "i-002", "kevlar", devPayload(map[string]any{"status": 401, "action": "auth"})),
		devRow(14, "2026-03-28T10:45:00 INFO vimgym i-003 deploy v2.3.1 started", []string{"2026-03-28T10:45:00", "INFO", "vimgym", "i-003", "deploy", "v2.3.1", "started"}, base.Add(45*time.Minute), "INFO", "vimgym", "i-003", "kevlar", devPayload(map[string]any{"status": 200, "action": "deploy", "version": "v2.3.1"})),
		devRow(15, "2026-03-28T10:50:00 INFO callbox i-002 connection pool resized to 20", []string{"2026-03-28T10:50:00", "INFO", "callbox", "i-002", "connection", "pool", "resized", "to", "20"}, base.Add(50*time.Minute), "INFO", "callbox", "i-002", "kevlar", devPayload(map[string]any{"status": 200, "action": "pool_resize", "size": 20})),
	}
}

func devShard1Rows() []*pb.LogRow {
	base := DevT1
	return []*pb.LogRow{
		devRow(16, "2026-03-28T11:00:01 INFO vimgym i-001 deploy v2.3.1 completed", []string{"2026-03-28T11:00:01", "INFO", "vimgym", "i-001", "deploy", "v2.3.1", "completed"}, base.Add(1*time.Second), "INFO", "vimgym", "i-001", "kevlar", devPayload(map[string]any{"status": 200, "action": "deploy", "version": "v2.3.1"})),
		devRow(17, "2026-03-28T11:01:00 ERROR vimgym i-001 500 internal server error on /api/users", []string{"2026-03-28T11:01:00", "ERROR", "vimgym", "i-001", "500", "internal", "server", "error", "on", "/api/users"}, base.Add(1*time.Minute), "ERROR", "vimgym", "i-001", "kevlar", devPayload(map[string]any{"status": 500, "action": "api", "path": "/api/users"})),
		devRow(18, "2026-03-28T11:05:00 WARN callbox i-002 disk usage above 80%", []string{"2026-03-28T11:05:00", "WARN", "callbox", "i-002", "disk", "usage", "above", "80%"}, base.Add(5*time.Minute), "WARN", "callbox", "i-002", "kevlar", devPayload(map[string]any{"status": 200, "action": "monitor", "disk_pct": 82})),
		devRow(19, "2026-03-28T11:10:00 INFO vimgym i-003 cache warmed 2500 keys", []string{"2026-03-28T11:10:00", "INFO", "vimgym", "i-003", "cache", "warmed", "2500", "keys"}, base.Add(10*time.Minute), "INFO", "vimgym", "i-003", "kevlar", devPayload(map[string]any{"status": 200, "action": "cache_warm", "keys": 2500})),
		devRow(20, "2026-03-28T11:15:00 ERROR callbox i-002 timeout connecting to redis", []string{"2026-03-28T11:15:00", "ERROR", "callbox", "i-002", "timeout", "connecting", "to", "redis"}, base.Add(15*time.Minute), "ERROR", "callbox", "i-002", "kevlar", devPayload(map[string]any{"status": 503, "action": "cache"})),
		devRow(21, "2026-03-28T11:20:00 INFO vimgym i-001 request completed", []string{"2026-03-28T11:20:00", "INFO", "vimgym", "i-001", "request", "completed"}, base.Add(20*time.Minute), "INFO", "vimgym", "i-001", "kevlar", devPayload(map[string]any{"status": 200, "action": "list", "duration_ms": 28})),
		devRow(22, "2026-03-28T11:25:00 WARN vimgym i-003 request latency spike p99=2.3s", []string{"2026-03-28T11:25:00", "WARN", "vimgym", "i-003", "request", "latency", "spike", "p99=2.3s"}, base.Add(25*time.Minute), "WARN", "vimgym", "i-003", "kevlar", devPayload(map[string]any{"status": 200, "action": "monitor", "p99_ms": 2300})),
		devRow(23, "2026-03-28T11:30:00 ERROR vimgym i-001 connection timeout to upstream", []string{"2026-03-28T11:30:00", "ERROR", "vimgym", "i-001", "connection", "timeout", "to", "upstream"}, base.Add(30*time.Minute), "ERROR", "vimgym", "i-001", "kevlar", devPayload(map[string]any{"status": 504, "action": "proxy"})),
		devRow(24, "2026-03-28T11:40:00 INFO callbox i-002 health check ok", []string{"2026-03-28T11:40:00", "INFO", "callbox", "i-002", "health", "check", "ok"}, base.Add(40*time.Minute), "INFO", "callbox", "i-002", "kevlar", devPayload(map[string]any{"status": 200, "action": "health"})),
		devRow(25, "2026-03-28T11:50:00 INFO vimgym i-003 graceful shutdown initiated", []string{"2026-03-28T11:50:00", "INFO", "vimgym", "i-003", "graceful", "shutdown", "initiated"}, base.Add(50*time.Minute), "INFO", "vimgym", "i-003", "kevlar", devPayload(map[string]any{"status": 200, "action": "shutdown"})),
	}
}
