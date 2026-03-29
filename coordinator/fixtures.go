package coordinator

import (
	"encoding/json"
	"time"

	"github.com/kevlarcon/weft/compute"
	"github.com/kevlarcon/weft/shard"
)

// DevFixtureTime constants for the sample data time windows.
var (
	DevT0 = time.Date(2026, 3, 28, 10, 0, 0, 0, time.UTC) // shard 0 start
	DevT1 = time.Date(2026, 3, 28, 11, 0, 0, 0, time.UTC) // shard 1 start
	DevT2 = time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC) // shard 1 end
)

func devPayload(m map[string]any) json.RawMessage {
	b, _ := json.Marshal(m)
	return b
}

// NewDevCoordinator creates a coordinator with two in-memory DuckDB shards
// populated with sample log data. Useful for development and demos.
// The caller must call the returned cleanup function when done.
func NewDevCoordinator() (*Coordinator, func(), error) {
	s0, err := shard.NewDuckDBShard("shard-0", "", DevT0, DevT1)
	if err != nil {
		return nil, nil, err
	}

	s1, err := shard.NewDuckDBShard("shard-1", "", DevT1, DevT2)
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

	c := &Coordinator{
		Shards:   []shard.Shard{s0, s1},
		Customer: "kevlar",
		Pool:     &compute.LocalComputePool{Parallelism: 2},
	}

	return c, cleanup, nil
}

// DevApps returns the list of app names available in the dev fixtures.
func DevApps() []string {
	return []string{"vimgym", "callbox"}
}

// DevShardRows returns fixture rows for the named shard ("shard-0" or "shard-1").
// Returns nil for unknown names.
func DevShardRows(name string) []shard.LogRow {
	switch name {
	case "shard-0":
		return devShard0Rows()
	case "shard-1":
		return devShard1Rows()
	}
	return nil
}

func devShard0Rows() []shard.LogRow {
	base := DevT0
	return []shard.LogRow{
		{LineNum: 1, Raw: "2026-03-28T10:00:01 ERROR vimgym i-001 connection timeout", Fields: []string{"2026-03-28T10:00:01", "ERROR", "vimgym", "i-001", "connection", "timeout"}, Ts: base.Add(1 * time.Second), Level: "ERROR", App: "vimgym", Instance: "i-001", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 500, "action": "login", "user_id": "u1"})},
		{LineNum: 2, Raw: "2026-03-28T10:00:05 INFO vimgym i-001 request completed", Fields: []string{"2026-03-28T10:00:05", "INFO", "vimgym", "i-001", "request", "completed"}, Ts: base.Add(5 * time.Second), Level: "INFO", App: "vimgym", Instance: "i-001", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 200, "action": "list", "duration_ms": 45})},
		{LineNum: 3, Raw: "2026-03-28T10:01:00 WARN callbox i-002 slow query detected", Fields: []string{"2026-03-28T10:01:00", "WARN", "callbox", "i-002", "slow", "query", "detected"}, Ts: base.Add(1 * time.Minute), Level: "WARN", App: "callbox", Instance: "i-002", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 200, "action": "search", "duration_ms": 1500})},
		{LineNum: 4, Raw: "2026-03-28T10:02:00 ERROR vimgym i-001 null pointer exception", Fields: []string{"2026-03-28T10:02:00", "ERROR", "vimgym", "i-001", "null", "pointer", "exception"}, Ts: base.Add(2 * time.Minute), Level: "ERROR", App: "vimgym", Instance: "i-001", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 500, "action": "update", "user_id": "u2"})},
		{LineNum: 5, Raw: "2026-03-28T10:03:00 INFO callbox i-002 health check ok", Fields: []string{"2026-03-28T10:03:00", "INFO", "callbox", "i-002", "health", "check", "ok"}, Ts: base.Add(3 * time.Minute), Level: "INFO", App: "callbox", Instance: "i-002", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 200, "action": "health"})},
		{LineNum: 6, Raw: "2026-03-28T10:05:00 ERROR callbox i-002 connection timeout to database", Fields: []string{"2026-03-28T10:05:00", "ERROR", "callbox", "i-002", "connection", "timeout", "to", "database"}, Ts: base.Add(5 * time.Minute), Level: "ERROR", App: "callbox", Instance: "i-002", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 503, "action": "query"})},
		{LineNum: 7, Raw: "2026-03-28T10:10:00 INFO vimgym i-003 user signup completed", Fields: []string{"2026-03-28T10:10:00", "INFO", "vimgym", "i-003", "user", "signup", "completed"}, Ts: base.Add(10 * time.Minute), Level: "INFO", App: "vimgym", Instance: "i-003", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 201, "action": "signup", "user_id": "u3"})},
		{LineNum: 8, Raw: "2026-03-28T10:15:00 DEBUG vimgym i-001 cache miss for key session:u1", Fields: []string{"2026-03-28T10:15:00", "DEBUG", "vimgym", "i-001", "cache", "miss", "for", "key", "session:u1"}, Ts: base.Add(15 * time.Minute), Level: "DEBUG", App: "vimgym", Instance: "i-001", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 200, "action": "cache_lookup"})},
		{LineNum: 9, Raw: "2026-03-28T10:20:00 ERROR vimgym i-003 timeout waiting for response", Fields: []string{"2026-03-28T10:20:00", "ERROR", "vimgym", "i-003", "timeout", "waiting", "for", "response"}, Ts: base.Add(20 * time.Minute), Level: "ERROR", App: "vimgym", Instance: "i-003", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 504, "action": "proxy"})},
		{LineNum: 10, Raw: "2026-03-28T10:25:00 INFO callbox i-002 batch job completed 150 records", Fields: []string{"2026-03-28T10:25:00", "INFO", "callbox", "i-002", "batch", "job", "completed", "150", "records"}, Ts: base.Add(25 * time.Minute), Level: "INFO", App: "callbox", Instance: "i-002", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 200, "action": "batch", "count": 150})},
		{LineNum: 11, Raw: "2026-03-28T10:30:00 WARN vimgym i-001 high memory usage 85%", Fields: []string{"2026-03-28T10:30:00", "WARN", "vimgym", "i-001", "high", "memory", "usage", "85%"}, Ts: base.Add(30 * time.Minute), Level: "WARN", App: "vimgym", Instance: "i-001", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 200, "action": "monitor", "memory_pct": 85})},
		{LineNum: 12, Raw: "2026-03-28T10:35:00 INFO vimgym i-001 request completed", Fields: []string{"2026-03-28T10:35:00", "INFO", "vimgym", "i-001", "request", "completed"}, Ts: base.Add(35 * time.Minute), Level: "INFO", App: "vimgym", Instance: "i-001", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 200, "action": "list", "duration_ms": 32})},
		{LineNum: 13, Raw: "2026-03-28T10:40:00 ERROR callbox i-002 authentication failed for tenant acme", Fields: []string{"2026-03-28T10:40:00", "ERROR", "callbox", "i-002", "authentication", "failed", "for", "tenant", "acme"}, Ts: base.Add(40 * time.Minute), Level: "ERROR", App: "callbox", Instance: "i-002", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 401, "action": "auth"})},
		{LineNum: 14, Raw: "2026-03-28T10:45:00 INFO vimgym i-003 deploy v2.3.1 started", Fields: []string{"2026-03-28T10:45:00", "INFO", "vimgym", "i-003", "deploy", "v2.3.1", "started"}, Ts: base.Add(45 * time.Minute), Level: "INFO", App: "vimgym", Instance: "i-003", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 200, "action": "deploy", "version": "v2.3.1"})},
		{LineNum: 15, Raw: "2026-03-28T10:50:00 INFO callbox i-002 connection pool resized to 20", Fields: []string{"2026-03-28T10:50:00", "INFO", "callbox", "i-002", "connection", "pool", "resized", "to", "20"}, Ts: base.Add(50 * time.Minute), Level: "INFO", App: "callbox", Instance: "i-002", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 200, "action": "pool_resize", "size": 20})},
	}
}

func devShard1Rows() []shard.LogRow {
	base := DevT1
	return []shard.LogRow{
		{LineNum: 16, Raw: "2026-03-28T11:00:01 INFO vimgym i-001 deploy v2.3.1 completed", Fields: []string{"2026-03-28T11:00:01", "INFO", "vimgym", "i-001", "deploy", "v2.3.1", "completed"}, Ts: base.Add(1 * time.Second), Level: "INFO", App: "vimgym", Instance: "i-001", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 200, "action": "deploy", "version": "v2.3.1"})},
		{LineNum: 17, Raw: "2026-03-28T11:01:00 ERROR vimgym i-001 500 internal server error on /api/users", Fields: []string{"2026-03-28T11:01:00", "ERROR", "vimgym", "i-001", "500", "internal", "server", "error", "on", "/api/users"}, Ts: base.Add(1 * time.Minute), Level: "ERROR", App: "vimgym", Instance: "i-001", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 500, "action": "api", "path": "/api/users"})},
		{LineNum: 18, Raw: "2026-03-28T11:05:00 WARN callbox i-002 disk usage above 80%", Fields: []string{"2026-03-28T11:05:00", "WARN", "callbox", "i-002", "disk", "usage", "above", "80%"}, Ts: base.Add(5 * time.Minute), Level: "WARN", App: "callbox", Instance: "i-002", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 200, "action": "monitor", "disk_pct": 82})},
		{LineNum: 19, Raw: "2026-03-28T11:10:00 INFO vimgym i-003 cache warmed 2500 keys", Fields: []string{"2026-03-28T11:10:00", "INFO", "vimgym", "i-003", "cache", "warmed", "2500", "keys"}, Ts: base.Add(10 * time.Minute), Level: "INFO", App: "vimgym", Instance: "i-003", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 200, "action": "cache_warm", "keys": 2500})},
		{LineNum: 20, Raw: "2026-03-28T11:15:00 ERROR callbox i-002 timeout connecting to redis", Fields: []string{"2026-03-28T11:15:00", "ERROR", "callbox", "i-002", "timeout", "connecting", "to", "redis"}, Ts: base.Add(15 * time.Minute), Level: "ERROR", App: "callbox", Instance: "i-002", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 503, "action": "cache"})},
		{LineNum: 21, Raw: "2026-03-28T11:20:00 INFO vimgym i-001 request completed", Fields: []string{"2026-03-28T11:20:00", "INFO", "vimgym", "i-001", "request", "completed"}, Ts: base.Add(20 * time.Minute), Level: "INFO", App: "vimgym", Instance: "i-001", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 200, "action": "list", "duration_ms": 28})},
		{LineNum: 22, Raw: "2026-03-28T11:25:00 WARN vimgym i-003 request latency spike p99=2.3s", Fields: []string{"2026-03-28T11:25:00", "WARN", "vimgym", "i-003", "request", "latency", "spike", "p99=2.3s"}, Ts: base.Add(25 * time.Minute), Level: "WARN", App: "vimgym", Instance: "i-003", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 200, "action": "monitor", "p99_ms": 2300})},
		{LineNum: 23, Raw: "2026-03-28T11:30:00 ERROR vimgym i-001 connection timeout to upstream", Fields: []string{"2026-03-28T11:30:00", "ERROR", "vimgym", "i-001", "connection", "timeout", "to", "upstream"}, Ts: base.Add(30 * time.Minute), Level: "ERROR", App: "vimgym", Instance: "i-001", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 504, "action": "proxy"})},
		{LineNum: 24, Raw: "2026-03-28T11:40:00 INFO callbox i-002 health check ok", Fields: []string{"2026-03-28T11:40:00", "INFO", "callbox", "i-002", "health", "check", "ok"}, Ts: base.Add(40 * time.Minute), Level: "INFO", App: "callbox", Instance: "i-002", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 200, "action": "health"})},
		{LineNum: 25, Raw: "2026-03-28T11:50:00 INFO vimgym i-003 graceful shutdown initiated", Fields: []string{"2026-03-28T11:50:00", "INFO", "vimgym", "i-003", "graceful", "shutdown", "initiated"}, Ts: base.Add(50 * time.Minute), Level: "INFO", App: "vimgym", Instance: "i-003", Customer: "kevlar", Payload: devPayload(map[string]any{"status": 200, "action": "shutdown"})},
	}
}
