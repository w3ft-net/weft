package app

import "flag"

// CoordinatorFlags are the coordinator's command-line flags. Their names,
// defaults and usage text are the process's interface to its unit files and
// deploy scripts; testdata/coordinator_flags.golden pins them.
type CoordinatorFlags struct {
	Port         int
	HealthPort   int
	LocalAddr    string
	InternalAddr string
	AdminPort    int
	DebugPort    int
	ComputeAddr  string
	RedisAddr    string
	RedisTLS     bool
	AWSRegion    string
	ShareURLBase string
	Customer     string
	DBDSN        string
	QueryLogPath string
}

// DefineCoordinatorFlags registers the coordinator's flags on fs.
func DefineCoordinatorFlags(fs *flag.FlagSet) *CoordinatorFlags {
	f := &CoordinatorFlags{}
	fs.IntVar(&f.Port, "port", 9020, "gRPC listen port")
	fs.IntVar(&f.HealthPort, "health-port", 9021, "HTTP liveness listen port")
	fs.StringVar(&f.LocalAddr, "local-addr", "127.0.0.1:9025", "listen address for the full WeftCoordinator service, which the co-located api connects to (the public --port serves only the ingest/infra RPCs)")
	fs.StringVar(&f.InternalAddr, "internal-addr", ":9026", "listen address for the VPC-internal Heartbeat port shards report to; reachable only through the internal NLB and its security group rule")
	fs.IntVar(&f.AdminPort, "admin-port", 9022, "gRPC listen port for the localhost-only WeftAdmin surface (weft-admin CLI)")
	fs.IntVar(&f.DebugPort, "debug-port", 6060, "localhost-only HTTP port for net/http/pprof (goroutine/heap profiling); 0 disables")
	fs.StringVar(&f.ComputeAddr, "compute", "", "compute worker address (host:port)")
	fs.StringVar(&f.RedisAddr, "redis-addr", "", "Redis address for cross-coordinator tail -f fan-out (host:port); empty disables cross-coordinator fan-out, tail stays local to this process")
	fs.BoolVar(&f.RedisTLS, "redis-tls", false, "use TLS for the Redis connection (required for ElastiCache in-transit encryption)")
	fs.StringVar(&f.AWSRegion, "region", "us-east-2", "AWS region for shard EC2 launches + volume management")
	fs.StringVar(&f.ShareURLBase, "share-url-base", "https://w3ft.net", "origin `clipboard` short links are built on")
	fs.StringVar(&f.Customer, "customer", "", "fallback customer for dev/docker (queries pass customer per-request)")
	fs.StringVar(&f.DBDSN, "db-dsn", "", "PostgreSQL DSN (e.g., host=localhost dbname=weft user=weft password=...)")
	fs.StringVar(&f.QueryLogPath, "query-log", "/var/log/weft/queries.log", "path for the query-audit log; falls back to stderr if unwritable")
	return f
}
