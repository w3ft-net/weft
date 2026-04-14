package shellfs

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kevlarcon/weft/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("WEFT_TEST_DSN")
	if dsn == "" {
		dsn = "host=localhost port=5433 dbname=weft user=weft password=weft sslmode=disable"
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	t.Cleanup(func() {
		db.TruncateTestTables()
		db.Close()
	})
	return db
}

func TestReadFile_EtcUsage(t *testing.T) {
	db := testStore(t)

	cust, err := db.FindOrCreateCustomer("usage-fs-test")
	if err != nil {
		t.Fatal(err)
	}

	fs := New(db, cust.ID, []string{"api"}, WithTier("swatch"), WithCustomerName("usage-fs-test"))

	data, err := fs.ReadFile("/etc/usage")
	if err != nil {
		t.Fatal(err)
	}

	content := string(data)

	// Should contain the header.
	if !strings.Contains(content, "# weft usage report") {
		t.Error("missing header")
	}
	if !strings.Contains(content, "# tier: swatch") {
		t.Errorf("missing tier line, got:\n%s", content)
	}
	// Should show ingestion line.
	if !strings.Contains(content, "ingested:") {
		t.Errorf("missing ingestion line, got:\n%s", content)
	}
}

func TestReadFile_EtcUsage_WithMetricsForecast(t *testing.T) {
	db := testStore(t)

	cust, err := db.FindOrCreateCustomer("usage-forecast-fs-test")
	if err != nil {
		t.Fatal(err)
	}

	// Create an agent with metrics enabled.
	ns, _ := db.FindOrCreateNamespace(cust.ID, "api")
	tok, err := db.CreateAgentToken(cust.ID, ns.ID, "metrics-agent")
	if err != nil {
		t.Fatal(err)
	}
	err = db.UpdateAgentMetricsConfig(tok.Token, true, "30s", []string{"cpu", "memory"})
	if err != nil {
		t.Fatal(err)
	}

	fs := New(db, cust.ID, []string{"api"}, WithTier("swatch"), WithCustomerName("usage-forecast-fs-test"))

	data, err := fs.ReadFile("/etc/usage")
	if err != nil {
		t.Fatal(err)
	}

	content := string(data)

	// Should include metrics forecast.
	if !strings.Contains(content, "metrics:") {
		t.Errorf("expected metrics forecast line, got:\n%s", content)
	}
	if !strings.Contains(content, "1 agent") {
		t.Errorf("expected '1 agent' in forecast, got:\n%s", content)
	}
	if !strings.Contains(content, "total:") {
		t.Errorf("expected total projection line, got:\n%s", content)
	}
}

func TestReadDir_CronD_ShowsSavedWatches(t *testing.T) {
	db := testStore(t)

	cust, err := db.FindOrCreateCustomer("crond-test")
	if err != nil {
		t.Fatal(err)
	}

	// Save a watch via the store.
	err = db.SaveCronWatch(cust.ID, "disk-alert", "every 5m", "cat /log/api.log | grep ERROR | wc -l | alert above 0", "")
	if err != nil {
		t.Fatal(err)
	}

	fs := New(db, cust.ID, []string{"api"}, WithTier("swatch"), WithCustomerName("crond-test"))

	// ls /cron.d/ should show the watch.
	entries, err := fs.ReadDir("/cron.d")
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, e := range entries {
		if e.Name() == "disk-alert" {
			found = true
		}
	}
	if !found {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected 'disk-alert' in /cron.d/ listing, got: %v", names)
	}

	// cat /cron.d/disk-alert should return the cron file.
	data, err := fs.ReadFile("/cron.d/disk-alert")
	if err != nil {
		t.Fatalf("ReadFile /cron.d/disk-alert: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "# schedule: every 5m") {
		t.Errorf("expected schedule directive, got:\n%s", content)
	}
	if !strings.Contains(content, "alert above 0") {
		t.Errorf("expected pipeline, got:\n%s", content)
	}
}

func TestReadDir_Etc_IncludesUsage(t *testing.T) {
	db := testStore(t)

	cust, err := db.FindOrCreateCustomer("etc-dir-test")
	if err != nil {
		t.Fatal(err)
	}

	fs := New(db, cust.ID, nil, WithTier("trial"))

	entries, err := fs.ReadDir("/etc")
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, e := range entries {
		if e.Name() == "usage" {
			found = true
			break
		}
	}
	if !found {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected 'usage' in /etc/ listing, got: %v", names)
	}
}

func TestReadFile_LogNamespace_PermissionDenied(t *testing.T) {
	db := testStore(t)

	cust, err := db.FindOrCreateCustomer("nsperm-test")
	if err != nil {
		t.Fatal(err)
	}

	// Create a namespace and a user with NO read permission.
	ns, err := db.FindOrCreateNamespace(cust.ID, "secret")
	if err != nil {
		t.Fatal(err)
	}
	email := fmt.Sprintf("restricted-%d@example.com", os.Getpid())
	user, err := db.CreateUser(email, cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	db.UpsertNamespacePermission(&store.NamespacePermission{
		UserID:      user.ID,
		NamespaceID: ns.ID,
		CanRead:     false,
	})

	fs := New(db, cust.ID, []string{"secret/app"},
		WithTier("swatch"),
		WithCustomerName("nsperm-test"),
		WithUser(user.ID, false))

	// ReadFile should return permission denied.
	_, err = fs.ReadFile("/log/secret/app.log")
	if err == nil {
		t.Fatal("expected permission denied")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected 'permission denied', got: %v", err)
	}
}

func TestReadDir_Log_FiltersNamespaces(t *testing.T) {
	db := testStore(t)

	cust, err := db.FindOrCreateCustomer("nsfilter-test")
	if err != nil {
		t.Fatal(err)
	}

	// Create two namespaces.
	nsA, _ := db.FindOrCreateNamespace(cust.ID, "allowed")
	nsB, _ := db.FindOrCreateNamespace(cust.ID, "blocked")
	_ = nsB

	// User has read on "allowed" but not "blocked".
	user, err := db.CreateUser(fmt.Sprintf("filtered-%d@example.com", os.Getpid()), cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	db.UpsertNamespacePermission(&store.NamespacePermission{
		UserID: user.ID, NamespaceID: nsA.ID, CanRead: true,
	})
	// No permission row for nsB = no access.

	fs := New(db, cust.ID, []string{"allowed/web", "blocked/api"},
		WithTier("swatch"),
		WithCustomerName("nsfilter-test"),
		WithUser(user.ID, false))

	entries, err := fs.ReadDir("/log")
	if err != nil {
		t.Fatal(err)
	}

	// Should only see "allowed/", not "blocked/".
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	if len(entries) != 1 || entries[0].Name() != "allowed" {
		t.Errorf("expected only 'allowed', got: %v", names)
	}
}

func TestReadFile_LogNamespace_OwnerBypass(t *testing.T) {
	db := testStore(t)

	cust, err := db.FindOrCreateCustomer("nsowner-test")
	if err != nil {
		t.Fatal(err)
	}
	db.FindOrCreateNamespace(cust.ID, "private")
	user, err := db.CreateUser(fmt.Sprintf("owner-%d@example.com", os.Getpid()), cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	// No NamespacePermission row — but user is owner.

	fs := New(db, cust.ID, []string{"private/app"},
		WithTier("swatch"),
		WithCustomerName("nsowner-test"),
		WithUser(user.ID, true)) // isOwner=true

	// Owner should have access despite no permission row.
	_, err = fs.ReadFile("/log/private/app.log")
	if err != nil {
		t.Errorf("owner should have access, got: %v", err)
	}
}

func TestStat_EtcUsage(t *testing.T) {
	db := testStore(t)

	cust, err := db.FindOrCreateCustomer("etc-stat-test")
	if err != nil {
		t.Fatal(err)
	}

	fs := New(db, cust.ID, nil, WithTier("trial"))

	info, err := fs.Stat("/etc/usage")
	if err != nil {
		t.Fatalf("stat /etc/usage: %v", err)
	}
	if info.Name() != "usage" {
		t.Errorf("expected name 'usage', got %q", info.Name())
	}
	if info.IsDir() {
		t.Error("expected file, got directory")
	}
}
