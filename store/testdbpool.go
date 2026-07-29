//go:build test

package store

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
)

// Pooled test databases. `go test ./...` runs each package's test binary
// as its own OS process, up to -p (default GOMAXPROCS) at a time. Pre-
// pool, every one of those processes hammered the same shared DB, and the
// lock contention surfaced as flaky 500s / multi-second query stalls
// across unrelated packages (auth sweeps timing out while a coordinator
// fan-out test held connections, etc.).
//
// The pool gives each process its own private database. On first use a
// process leases a free slot by taking an exclusive flock on a per-slot
// lockfile, then runs against weft_test_<slot>. The flock is held for the
// process's whole lifetime (the fd never closes until exit), so a second
// process can't pick the same slot; the kernel releases it automatically
// when the process dies, even on a panic or SIGKILL — no stale-lock
// cleanup to get wrong.
//
// Within a single process tests still share one DB, exactly as before, so
// the store package's tx-rollback isolation (BeginTest) and the
// real-DB deleteOrgData scoping both keep working unchanged. The win
// is purely cross-package.

// testDSNBase is the connection target every pooled DB shares; only the
// dbname varies per slot. Matches the dev docker-compose postgres
// (docker-compose.yml maps 5432→5433).
const testDSNBase = "host=localhost port=5433 user=weft password=weft sslmode=disable dbname=postgres"

var (
	leaseOnce sync.Once
	leasedDSN string
	// leaseFile holds the flock for the process lifetime. Kept in a
	// package var so the GC can't finalize+close the fd and drop the
	// lock out from under us mid-run.
	leaseFile *os.File
)

// testPoolSize is how many databases the pool spans. Defaults to
// GOMAXPROCS (which is what `go test` uses for -p, so a process never
// has to block waiting for a slot in the common case), overridable via
// WEFT_TEST_DB_POOL for machines that want to cap concurrency.
func testPoolSize() int {
	if v := os.Getenv("WEFT_TEST_DB_POOL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		n = 1
	}
	return n
}

// TestDSN returns the database DSN this test process should use. The
// first call leases a private pool database (creating + migrating it if
// needed) and holds the lease until the process exits; later calls return
// the same DSN. An explicit WEFT_TEST_DSN bypasses the pool entirely —
// CI and manual overrides keep pointing tests wherever they like.
//
// On any failure to reach or prepare postgres it skips the test, matching
// the long-standing "postgres unavailable → skip, don't fail" convention
// of the individual helpers this replaces.
func TestDSN(t testing.TB) string {
	t.Helper()
	if dsn := os.Getenv("WEFT_TEST_DSN"); dsn != "" {
		return dsn
	}
	leaseOnce.Do(func() {
		leasedDSN = leaseTestDB(t)
	})
	if leasedDSN == "" {
		t.Skipf("postgres unavailable: could not lease a pool database")
	}
	return leasedDSN
}

// leaseTestDB picks a free pool slot, prepares its database, and returns
// the DSN. Returns "" (→ skip) if postgres can't be reached. Runs once
// per process under leaseOnce.
func leaseTestDB(t testing.TB) string {
	n := testPoolSize()
	dir := os.TempDir()

	// First pass: grab any immediately-free slot without blocking.
	for i := 0; i < n; i++ {
		if f, ok := tryLockSlot(dir, i); ok {
			return prepareSlot(t, i, f)
		}
	}
	// Every slot is busy (more concurrent package binaries than slots).
	// Block on a pid-spread slot so waiters don't all pile onto slot 0.
	i := os.Getpid() % n
	f, err := lockSlotBlocking(dir, i)
	if err != nil {
		return ""
	}
	return prepareSlot(t, i, f)
}

// slotDSN is the DSN for pool slot i (weft_test_<i>).
func slotDSN(i int) string {
	return replaceDBName(testDSNBase, "weft_test_"+strconv.Itoa(i))
}

// tryLockSlot attempts a non-blocking exclusive flock on slot i's
// lockfile. Returns the held file (caller must keep it open) and true on
// success; (nil, false) if the slot is taken or the lockfile can't be
// opened.
func tryLockSlot(dir string, i int) (*os.File, bool) {
	f, err := os.OpenFile(lockPath(dir, i), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, false
	}
	return f, true
}

// lockSlotBlocking takes an exclusive flock on slot i, waiting until it's
// free. Used only when every slot was busy on the non-blocking pass.
func lockSlotBlocking(dir string, i int) (*os.File, error) {
	f, err := os.OpenFile(lockPath(dir, i), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func lockPath(dir string, i int) string {
	return filepath.Join(dir, "weft-test-db-"+strconv.Itoa(i)+".lock")
}

// prepareSlot creates slot i's database if absent and migrates it to the
// current schema, then records the held lock and returns the DSN. A
// preparation failure releases the lock and returns "" so the caller
// skips rather than running against a half-built DB.
func prepareSlot(t testing.TB, i int, f *os.File) string {
	dsn := slotDSN(i)
	if err := ensureTestDatabase(dsn); err != nil {
		f.Close()
		return ""
	}
	// Pool DBs aren't pre-bootstrapped like the dev stack's `weft`, so
	// migrate them here. Migrate refuses to apply schema.sql on a virgin
	// DB unless the bootstrap gate is set; set it process-wide (this is a
	// test process) so the first lease of each slot builds the schema and
	// later leases are idempotent no-ops.
	os.Setenv(AllowSchemaBootstrapEnv, "1")
	if err := Migrate(dsn); err != nil {
		f.Close()
		return ""
	}
	leaseFile = f
	return dsn
}
