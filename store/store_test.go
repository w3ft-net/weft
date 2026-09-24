package store

import "testing"

// TestOpen_ConfiguresBoundedConnectionPool pins the fix for a real
// production incident (2026-09-24): Open never called SetMaxOpenConns,
// so database/sql's default (unlimited) applied. With coordinator_
// desired=2 — each instance running both a coordinator and an api
// process, all four talking to the same db.t4g.micro RDS instance
// (max_connections ~112) — a concurrency burst opened enough
// connections at once (observed spike: 117) to exhaust the instance,
// taking shard-heartbeat persistence down with "sorry, too many
// clients already". An unbounded pool on any one process was always
// one bad burst away from this; the fix caps every process's own
// contribution regardless of what triggers the burst.
func TestOpen_ConfiguresBoundedConnectionPool(t *testing.T) {
	dsn := TestDSN(t)
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	sqlDB, err := s.db.DB()
	if err != nil {
		t.Fatalf("unwrap *sql.DB: %v", err)
	}
	stats := sqlDB.Stats()
	if stats.MaxOpenConnections != dbMaxOpenConnsDefault {
		t.Errorf("MaxOpenConnections = %d, want the configured default %d (database/sql's own default is 0 = unlimited)",
			stats.MaxOpenConnections, dbMaxOpenConnsDefault)
	}
}

// TestOpen_MaxOpenConnsEnvOverride confirms DB_MAX_OPEN_CONNS can
// override the default without a code change, for when the process
// topology changes (more coordinator instances, a new DB client).
func TestOpen_MaxOpenConnsEnvOverride(t *testing.T) {
	t.Setenv("DB_MAX_OPEN_CONNS", "7")
	dsn := TestDSN(t)
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	sqlDB, err := s.db.DB()
	if err != nil {
		t.Fatalf("unwrap *sql.DB: %v", err)
	}
	if got := sqlDB.Stats().MaxOpenConnections; got != 7 {
		t.Errorf("MaxOpenConnections = %d, want 7 (from DB_MAX_OPEN_CONNS)", got)
	}
}

// TestOpen_MaxOpenConnsInvalidEnvFallsBackToDefault: a malformed or
// non-positive override must not silently disable the cap (0 would
// mean "unlimited" to database/sql — reintroducing the exact bug this
// is meant to prevent) or crash Open.
func TestOpen_MaxOpenConnsInvalidEnvFallsBackToDefault(t *testing.T) {
	for _, v := range []string{"not-a-number", "0", "-5"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("DB_MAX_OPEN_CONNS", v)
			dsn := TestDSN(t)
			s, err := Open(dsn)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer s.Close()

			sqlDB, err := s.db.DB()
			if err != nil {
				t.Fatalf("unwrap *sql.DB: %v", err)
			}
			if got := sqlDB.Stats().MaxOpenConnections; got != dbMaxOpenConnsDefault {
				t.Errorf("MaxOpenConnections = %d, want fallback to default %d for invalid DB_MAX_OPEN_CONNS=%q",
					got, dbMaxOpenConnsDefault, v)
			}
		})
	}
}
