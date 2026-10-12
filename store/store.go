package store

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/graybeardcorp/saaskit/orgs"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// dbMaxOpenConnsDefault caps how many concurrent Postgres connections a
// single process holds open. database/sql's own default is 0 (unlimited)
// — Open never called SetMaxOpenConns at all until this was added, which
// let a 2026-09-24 concurrency burst push the shared db.t4g.micro RDS
// instance (max_connections ~112, LEAST(memory/9531392, 5000)) to 117
// connections and take shard-heartbeat persistence down with "sorry, too
// many clients already". Four processes share that instance today
// (coordinator_desired=2, each instance running both a coordinator and
// an api process); 20 × 4 = 80 leaves headroom for ad hoc tools
// (weft-admin, weft-migrate, weft-demo-seed) and RDS's reserved
// superuser slots. Override via DB_MAX_OPEN_CONNS if that topology
// changes rather than editing this constant.
const dbMaxOpenConnsDefault = 20

// maxOpenConns resolves DB_MAX_OPEN_CONNS if set to a positive integer,
// else dbMaxOpenConnsDefault. A non-positive override is rejected rather
// than silently accepted — 0 means "unlimited" to database/sql, which
// would reintroduce the exact bug this cap exists to prevent.
func maxOpenConns() int {
	if v := os.Getenv("DB_MAX_OPEN_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return dbMaxOpenConnsDefault
}

// MigrateDSN returns the DSN to run migrations over: $DB_MIGRATE_DSN (a
// direct connection to RDS, bypassing PgBouncer) if set, else dsn.
func MigrateDSN(dsn string) string {
	if v := os.Getenv("DB_MIGRATE_DSN"); v != "" {
		return v
	}
	return dsn
}

// Store manages all weft PostgreSQL state in a single unified package.
type Store struct {
	db *gorm.DB

	// tenant holds the enforced multi-tenancy registry + GORM
	// callbacks (see tenant.go). Shared by pointer across a parent
	// Store and the transaction-Stores BeginTest derives from it, so
	// model enrollment and the scoping callbacks read the same set.
	tenant *tenantScoping

	// Orgs owns organizations + memberships, shared with vimgym via
	// saaskit/orgs. It runs on the same *sql.DB gorm does, beneath the
	// tenant-scoping callbacks — so its queries carry org_id explicitly
	// rather than picking it up from context.
	Orgs *orgs.Store

	// Test-only: raw magic-link tokens indexed by email. Populated by
	// CreateMagicLink; read by GetLastMagicLinkToken. The tokens
	// themselves are hashed before being persisted, so this map is the
	// only way an in-process test can retrieve the raw value after
	// sending it through the handler. In production nothing reads from
	// this map, and tokens are cleared every 15 minutes by normal
	// consumption / expiry.
	rawMagicLinkMu     sync.Mutex
	rawMagicLinkTokens map[string]string

	// schemaCache is the optional Valkey-backed cache-aside layer in
	// front of log_groups.schema_yaml lookups (schema_cache.go). Nil
	// unless SetSchemaCache is called — every SchemaCache method is
	// nil-receiver-safe, so callers that never wire one (tests,
	// weft-admin, weft-migrate) transparently always fall through to
	// Postgres.
	schemaCache *SchemaCache

	// authCache is the optional in-process ValidateAgentToken cache
	// (auth_cache.go). Nil unless EnableAuthCache is called, so tests and
	// CLIs that never opt in always read through to Postgres.
	authCache *authCache
}

// SetSchemaCache attaches (or, passed nil, detaches) the shared Valkey
// schema cache. Separate from Open because only api and coordinator
// processes have a Redis address to offer; every other Store consumer
// (tests, weft-admin, weft-migrate, weft-demo-seed) just never calls
// this and gets Postgres-only behavior.
func (s *Store) SetSchemaCache(c *SchemaCache) {
	s.schemaCache = c
}

// Open ensures all goose migrations have been applied, then connects.
// Schema is authoritative from the .sql files under store/migrations; we no
// longer use GORM AutoMigrate.
func Open(dsn string) (*Store, error) {
	if err := Migrate(dsn); err != nil {
		return nil, err
	}
	return Connect(dsn)
}

// Connect connects without migrating. Prod processes migrate over a direct
// connection to RDS (MigrateDSN) and connect through PgBouncer: Migrate's
// session-scoped advisory lock and the schema.sql restore's session
// settings would stay behind on a transaction-mode pooler's server
// connections.
func Connect(dsn string) (*Store, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
		// A single INSERT/UPDATE/DELETE is already atomic; GORM's default
		// BEGIN/COMMIT wrap would add two round trips per write. Nothing here
		// relies on it: there are no model hooks, no association writes, and
		// every multi-statement path (including CreateInBatches over several
		// batches) runs inside an explicit Transaction.
		SkipDefaultTransaction: true,
	})
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("store: unwrap *sql.DB for orgs: %w", err)
	}
	sqlDB.SetMaxOpenConns(maxOpenConns())
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	s := &Store{db: db, tenant: newTenantScoping(), Orgs: orgs.New(sqlDB)}
	s.tenant.install(db)
	// Declare which models are tenant-scoped. Enforcement runs through
	// the scoping callbacks installed just above; models are enrolled
	// as their call sites learn to pass the customer through context.
	s.tenant.enroll(s.db,
		&Dashboard{},
		&ShellFunction{},
		&OrgPermission{},
		&NotifyConfig{},
		&PagerDutyIntegration{},
		&CronWatch{},
		&TmpFile{},
		&Subscription{},
		&AgentToken{},
		&Namespace{},
		&SharedLink{},
	)
	// One-time backfill of (alert_condition, alert_threshold) for
	// watches that predate migration 027 (the columns were added
	// nullable, so the migration itself is instant). This second pass
	// walks rows with NULL values, re-compiles the pipeline, and writes
	// the projection. Idempotent: once every eligible row has both
	// columns set the SELECT returns zero and this is a no-op.
	// Unparseable pipelines are skipped so a bad row can't block startup.
	{
		type row struct {
			ID       uint
			Pipeline string
		}
		var rows []row
		if err := s.db.Raw(`
			SELECT id, pipeline
			FROM cron_watches
			WHERE deleted_at IS NULL
			  AND alert_condition IS NULL
			  AND alert_threshold IS NULL
		`).Scan(&rows).Error; err != nil {
			return nil, fmt.Errorf("backfill alert thresholds: %w", err)
		}
		backfilled := 0
		for _, r := range rows {
			compiled, err := CompileWatchPipeline(r.Pipeline)
			if err != nil {
				// A pipeline that no longer parses (or has multiple
				// alerts, etc.) is a pre-existing bad row — don't let
				// it block server startup. Leave the columns NULL.
				continue
			}
			if compiled.AlertCondition == nil || compiled.AlertThreshold == nil {
				// Watch has no numeric-cutoff alert. Nothing to project.
				continue
			}
			if err := s.db.Exec(`
				UPDATE cron_watches
				SET alert_condition = ?, alert_threshold = ?
				WHERE id = ?`,
				*compiled.AlertCondition, *compiled.AlertThreshold, r.ID,
			).Error; err != nil {
				return nil, fmt.Errorf("backfill alert thresholds: %w", err)
			}
			backfilled++
		}
		if backfilled > 0 {
			log.Printf("store: backfilled alert_condition/alert_threshold on %d cron_watches", backfilled)
		}
	}
	return s, nil
}

// BeginTest starts a transaction and returns a Store that uses it.
// Call the returned rollback function in t.Cleanup to undo all changes.
//
// The org store is rebound to the same transaction rather than the pool it
// normally holds. orgs runs on database/sql beneath gorm, so a Store that kept
// the pool here would write memberships outside the transaction and survive
// the rollback — the rows would then leak into whatever test ran next.
func (s *Store) BeginTest() (*Store, func()) {
	tx := s.db.Begin()
	orgStore := s.Orgs
	if sqlTx, ok := tx.Statement.ConnPool.(*sql.Tx); ok {
		orgStore = orgs.New(sqlTx)
	}
	return &Store{db: tx, tenant: s.tenant, Orgs: orgStore}, func() { tx.Rollback() }
}

// TruncateTestTables removes all data from GORM-managed tables. For tests only.
func (s *Store) TruncateTestTables() {
	// Discarding this error once cost a debugging cycle: when the statement
	// named a table that no longer existed the truncate silently did nothing,
	// and the damage surfaced as unrelated duplicate-key failures in whichever
	// test happened to run next. A broken fixture should say so.
	if err := s.db.Exec("TRUNCATE TABLE cron_results, cron_watches, shell_functions, agent_tokens, notify_configs, namespace_permissions, org_permissions, namespaces, alert_incidents, device_codes, magic_links, memberships, shard_records, subscriptions, users, organizations CASCADE").Error; err != nil {
		panic(fmt.Sprintf("store: TruncateTestTables: %v", err))
	}
}

// OnSQLForTest calls fn with the text of every statement the store runs
// (queries, updates, creates, raw execs) until the returned remove func is
// called. Test-only: for asserting that a path does or does not touch the
// database. name must be unique per registration.
func (s *Store) OnSQLForTest(name string, fn func(sql string)) (remove func()) {
	cb := func(db *gorm.DB) { fn(db.Statement.SQL.String()) }
	cbs := s.db.Callback()
	cbs.Query().After("gorm:query").Register(name+":query", cb)
	cbs.Create().After("gorm:create").Register(name+":create", cb)
	cbs.Update().After("gorm:update").Register(name+":update", cb)
	cbs.Raw().After("gorm:raw").Register(name+":raw", cb)
	return func() {
		cbs.Query().Remove(name + ":query")
		cbs.Create().Remove(name + ":create")
		cbs.Update().Remove(name + ":update")
		cbs.Raw().Remove(name + ":raw")
	}
}

// ExecForTest runs raw SQL on the store's connection. Test-only:
// most production paths should go through GORM methods. Use this
// for scoped cleanup that doesn't have a single Store method (e.g.
// "delete everything owned by customer X").
func (s *Store) ExecForTest(sql string, args ...any) error {
	return s.db.Exec(sql, args...).Error
}

// Close releases the database connection.
func (s *Store) Close() error {
	db, err := s.db.DB()
	if err != nil {
		return err
	}
	return db.Close()
}
