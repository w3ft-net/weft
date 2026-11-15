package options

import "flag"

// Flags are the api's command-line flags. Their names, defaults and usage text
// are the process's interface to its unit files; testdata/api_flags.golden pins
// them.
type Flags struct {
	Port             string
	DBDSN            string
	CoordinatorAddr  string
	BaseURL          string
	RenderSyslogPort int
	RedisAddr        string
	RedisTLS         bool
}

// Define registers the api's flags on fs.
func Define(fs *flag.FlagSet) *Flags {
	f := &Flags{}
	fs.StringVar(&f.Port, "port", "8080", "HTTP listen port")
	fs.StringVar(&f.DBDSN, "db-dsn", "", "PostgreSQL DSN")
	fs.StringVar(&f.CoordinatorAddr, "coordinator", "localhost:9025", "coordinator gRPC address (its --local-addr; the public port serves only ingest RPCs)")
	fs.StringVar(&f.BaseURL, "base-url", "https://w3ft.net", "base URL for magic links")
	fs.IntVar(&f.RenderSyslogPort, "render-syslog-port", 9023, "TCP port for the Render TLS syslog listener (see docs/ingestion-syslog.md); the NLB terminates TLS and forwards plaintext here")
	fs.StringVar(&f.RedisAddr, "redis-addr", "", "Redis/Valkey address for the shared ingest-schema cache (host:port); empty disables it, every schema lookup falls through to Postgres directly")
	fs.BoolVar(&f.RedisTLS, "redis-tls", false, "use TLS for the Redis connection (required for ElastiCache in-transit encryption)")
	return f
}
