package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

// setupSource creates a customer + namespace and returns the store, the
// namespace id, the customer id, and the (namespace, app, fullApp) names.
func setupSource(t *testing.T, name string) (db *Store, nsID uint, custID OrgID, nsName, app, fullApp string) {
	t.Helper()
	db = testOpen(t)
	cust, err := db.FindOrCreateOrg(name)
	if err != nil {
		t.Fatal(err)
	}
	nsName = "ns"
	ns, err := db.FindOrCreateNamespace(cust.ID, nsName)
	if err != nil {
		t.Fatal(err)
	}
	app = "svc"
	// log_groups.app stores the bare service (migration 060); the
	// namespace scopes it via namespace_id. fullApp is the bare service
	// too — kept as a distinct name only to minimise churn in callers.
	fullApp = app
	return db, ns.ID, cust.ID, nsName, app, fullApp
}

// TestSamplingThenPendingProgression: a partial submit registers the
// source as "sampling" with progress; the final submit promotes it to
// "pending" (configurable).
func TestSamplingThenPendingProgression(t *testing.T) {
	db, nsID, custID, nsName, app, fullApp := setupSource(t, "resample-progress")

	if err := db.UpsertPendingLogGroup(nsID, fullApp, "host1", "dep", "[]", "null", 0, true); err != nil {
		t.Fatalf("partial upsert: %v", err)
	}
	lg, err := db.FindLogGroup(custID, nsName, app)
	if err != nil || lg == nil {
		t.Fatalf("FindLogGroup after partial: %v lg=%v", err, lg)
	}
	if lg.Status != StatusSampling {
		t.Fatalf("expected status=sampling, got %q", lg.Status)
	}

	if err := db.UpsertPendingLogGroup(nsID, fullApp, "host1", "dep", `["a","b"]`, "{}", 2, true); err != nil {
		t.Fatalf("partial upsert 2: %v", err)
	}
	lg, _ = db.FindLogGroup(custID, nsName, app)
	if lg.Status != StatusSampling || lg.SampleCollected != 2 {
		t.Fatalf("expected sampling with collected=2, got status=%q collected=%d", lg.Status, lg.SampleCollected)
	}

	// Final submit → pending, configurable.
	if err := db.UpsertPendingLogGroup(nsID, fullApp, "host1", "dep", `["a","b","c"]`, "{}", 3, false); err != nil {
		t.Fatalf("final upsert: %v", err)
	}
	lg, _ = db.FindLogGroup(custID, nsName, app)
	if lg.Status != StatusPending {
		t.Fatalf("expected status=pending after final submit, got %q", lg.Status)
	}
	pend, err := func() (*LogGroup, error) {
		var lg LogGroup
		err := db.DB().Where("namespace_id = ? AND app = ? AND status IN ?",
			nsID, fullApp, []LogGroupStatus{StatusSampling, StatusPending}).First(&lg).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &lg, nil
	}()
	if err != nil || pend == nil {
		t.Fatalf("get pending log group after final: %v pend=%v", err, pend)
	}
}

// TestResampleKeepsConfiguredAndSwaps: the core requirement. A resample
// on a live configured source keeps it configured (ingestion uninterrupted)
// while a fresh sample lands; a no-resample sample is a no-op; the final
// resample submit clears the flag; reconfiguring swaps the schema in place.
func TestResampleKeepsConfiguredAndSwaps(t *testing.T) {
	db, nsID, custID, nsName, app, fullApp := setupSource(t, "resample-swap")

	if err := db.UpsertPendingLogGroup(nsID, fullApp, "h", "d", `["x"]`, "{}", 1, false); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := configureLogGroup(db, nsID, fullApp, "schema-v1"); err != nil {
		t.Fatalf("configure: %v", err)
	}

	// A stray sample with NO resample requested is a no-op: schema and
	// status preserved, no sample stored.
	if err := db.UpsertPendingLogGroup(nsID, fullApp, "h", "d", `["stale"]`, "{}", 1, false); err != nil {
		t.Fatalf("stale upsert: %v", err)
	}
	lg, _ := db.FindLogGroup(custID, nsName, app)
	if lg.Status != StatusConfigured || lg.SchemaYAML != "schema-v1" || lg.Sample != "" {
		t.Fatalf("stale sample must be a no-op; got status=%q schema=%q sample=%q",
			lg.Status, lg.SchemaYAML, lg.Sample)
	}

	// Request resample, then a partial submit: sample stored, still
	// configured + still ingesting, flag still set.
	if err := db.RequestResample(nsID, fullApp); err != nil {
		t.Fatalf("RequestResample: %v", err)
	}
	if err := db.UpsertPendingLogGroup(nsID, fullApp, "h", "d", `["fresh1"]`, "{}", 1, true); err != nil {
		t.Fatalf("partial resample upsert: %v", err)
	}
	lg, _ = db.FindLogGroup(custID, nsName, app)
	if lg.Status != StatusConfigured {
		t.Fatalf("resample-in-flight must stay configured, got %q", lg.Status)
	}
	if lg.SchemaYAML != "schema-v1" {
		t.Fatalf("schema must be unchanged during resample, got %q", lg.SchemaYAML)
	}
	if lg.Sample != `["fresh1"]` {
		t.Fatalf("fresh sample should be stored, got %q", lg.Sample)
	}
	if lg.ResampleRequestedAt == nil {
		t.Fatal("partial submit must NOT clear the resample flag")
	}

	// Final resample submit clears the flag.
	if err := db.UpsertPendingLogGroup(nsID, fullApp, "h", "d", `["fresh1","fresh2"]`, "{}", 2, false); err != nil {
		t.Fatalf("final resample upsert: %v", err)
	}
	lg, _ = db.FindLogGroup(custID, nsName, app)
	if lg.ResampleRequestedAt != nil {
		t.Fatal("final submit must clear the resample flag")
	}
	if lg.Status != StatusConfigured {
		t.Fatalf("still configured after resample complete, got %q", lg.Status)
	}

	// Reconfigure swaps the schema in place (still configured, sample
	// cleared) — the non-disruptive switchover.
	if err := configureLogGroup(db, nsID, fullApp, "schema-v2"); err != nil {
		t.Fatalf("reconfigure after resample: %v", err)
	}
	lg, _ = db.FindLogGroup(custID, nsName, app)
	if lg.Status != StatusConfigured || lg.SchemaYAML != "schema-v2" {
		t.Fatalf("schema swap failed: status=%q schema=%q", lg.Status, lg.SchemaYAML)
	}
	if lg.Sample != "" || lg.ResampleRequestedAt != nil {
		t.Fatalf("reconfigure must clear sample + resample flag; sample=%q flag=%v",
			lg.Sample, lg.ResampleRequestedAt)
	}
}

// --- helpers for log-group store methods relocated out of the store package ---
//
// Production logic moved to callers outside the store package. These
// test-local helpers reproduce the original method bodies verbatim
// (parameterized on db) so the resample tests keep exercising the
// same behaviour.

// configureLogGroup mirrors the former Store.ConfigureLogGroup.
func configureLogGroup(db *Store, namespaceID uint, app, schemaYAML string) error {
	res := db.db.Model(&LogGroup{}).
		Where("namespace_id = ? AND app = ? AND status IN ?",
			namespaceID, app, []LogGroupStatus{StatusPending, StatusConfigured}).
		Updates(map[string]any{
			"status":                StatusConfigured,
			"schema_yaml":           schemaYAML,
			"sample":                nil,
			"detection":             nil,
			"sample_collected":      0,
			"resample_requested_at": nil,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// A pending source can be resampled: the first batch (startup chatter) may
// not show every value worth extracting. The row goes back to sampling with
// an empty sample, and the flag tells a waiting agent to collect fresh lines.
func TestRequestResampleOnPendingRestartsSampling(t *testing.T) {
	db, nsID, custID, nsName, app, fullApp := setupSource(t, "resample-pending")
	if err := db.UpsertPendingLogGroup(nsID, fullApp, "h", "d", `["boot 1","boot 2"]`, `{"x":1}`, 50, false); err != nil {
		t.Fatal(err)
	}
	if err := db.RequestResample(nsID, fullApp); err != nil {
		t.Fatalf("RequestResample on pending: %v", err)
	}
	lg, _ := db.FindLogGroup(custID, nsName, app)
	if lg.Status != StatusSampling {
		t.Errorf("status = %q, want sampling", lg.Status)
	}
	if lg.Sample != "" || lg.Detection != "" || lg.SampleCollected != 0 {
		t.Errorf("sample not cleared: sample=%q detection=%q collected=%d", lg.Sample, lg.Detection, lg.SampleCollected)
	}
	if lg.ResampleRequestedAt == nil {
		t.Error("ResampleRequestedAt not set")
	}

	// The fresh final sample ends the resample and is pending again.
	if err := db.UpsertPendingLogGroup(nsID, fullApp, "h", "d", `["fresh"]`, "{}", 1, false); err != nil {
		t.Fatal(err)
	}
	lg, _ = db.FindLogGroup(custID, nsName, app)
	if lg.Status != StatusPending || lg.ResampleRequestedAt != nil {
		t.Errorf("after final sample: status=%q flag=%v, want pending and cleared", lg.Status, lg.ResampleRequestedAt)
	}
}

// A source that is still filling its first sample is already collecting, and
// a missing one has nothing to resample.
func TestRequestResampleGuards(t *testing.T) {
	db, nsID, _, _, _, fullApp := setupSource(t, "resample-guards")
	if err := db.RequestResample(nsID, fullApp); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("missing source: want ErrRecordNotFound, got %v", err)
	}
	if err := db.UpsertPendingLogGroup(nsID, fullApp, "h", "d", `["x"]`, "{}", 1, true); err != nil {
		t.Fatal(err)
	}
	if err := db.RequestResample(nsID, fullApp); !errors.Is(err, ErrAlreadySampling) {
		t.Fatalf("sampling source: want ErrAlreadySampling, got %v", err)
	}
}

// A direct (Heroku/Render) source that already holds a full sample must have
// its count reset on resample, or the ingest path (which stops at the target)
// never collects a new one.
func TestRequestResampleResetsDirectSample(t *testing.T) {
	db, nsID, custID, nsName, app, _ := setupSource(t, "resample-direct")
	if err := db.EnsureServiceLogGroup(nsID, app); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertPendingLogGroup(nsID, app, "", "", `["a"]`, "{}", SampleTarget, false); err != nil {
		t.Fatal(err)
	}
	if err := db.RequestResample(nsID, app); err != nil {
		t.Fatal(err)
	}
	lg, _ := db.FindLogGroup(custID, nsName, app)
	if lg.Status != StatusConfigured {
		t.Errorf("status = %q, want configured (ingestion never pauses)", lg.Status)
	}
	if lg.SampleCollected != 0 || lg.Sample != "" || lg.ResampleRequestedAt == nil {
		t.Errorf("collected=%d sample=%q flag=%v, want reset and flagged", lg.SampleCollected, lg.Sample, lg.ResampleRequestedAt)
	}
}

// A resample already collecting must not be restarted by a second request:
// for a direct source the row is "configured" the whole time, so the guard
// has to look at the flag, not just the status.
func TestRequestResampleWhileCollectingConflicts(t *testing.T) {
	db, nsID, custID, nsName, app, _ := setupSource(t, "resample-inflight")
	if err := db.EnsureServiceLogGroup(nsID, app); err != nil { // created collecting
		t.Fatal(err)
	}
	if err := db.UpsertPendingLogGroup(nsID, app, "", "", `["a","b"]`, "{}", 2, true); err != nil {
		t.Fatal(err)
	}
	if err := db.RequestResample(nsID, app); !errors.Is(err, ErrAlreadySampling) {
		t.Fatalf("resample while collecting = %v, want ErrAlreadySampling", err)
	}
	lg, _ := db.FindLogGroup(custID, nsName, app)
	if lg.SampleCollected != 2 {
		t.Errorf("collected = %d, want 2: a refused request must not reset progress", lg.SampleCollected)
	}

	// Once the sample is full the flag clears and a new resample is accepted.
	if err := db.UpsertPendingLogGroup(nsID, app, "", "", `["a","b"]`, "{}", SampleTarget, false); err != nil {
		t.Fatal(err)
	}
	if err := db.RequestResample(nsID, app); err != nil {
		t.Fatalf("resample after a full sample: %v", err)
	}
}

// A configured weftd source with a schema that is being resampled is awaiting
// the user: it must be listed while the sample fills (flag set) and after it
// completes (flag cleared, fresh sample waiting for review), and drop off the
// moment the user reconfigures it. Without this, a resample is invisible in
// `weft source pending`.
func TestAwaitingSchemaListsResampledConfiguredSource(t *testing.T) {
	db, nsID, custID, _, _, fullApp := setupSource(t, "awaiting-resampled")
	if err := db.UpsertPendingLogGroup(nsID, fullApp, "h", "d", `["x"]`, "{}", 1, false); err != nil {
		t.Fatal(err)
	}
	if err := configureLogGroup(db, nsID, fullApp, "schema-v1"); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		n, err := db.CountPendingLogGroups(custID)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 0 {
		t.Fatalf("configured source with no resample listed: count=%d", n)
	}

	if err := db.RequestResample(nsID, fullApp); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Errorf("resample requested, nothing collected yet: count=%d, want 1", n)
	}

	if err := db.UpsertPendingLogGroup(nsID, fullApp, "h", "d", `["f1"]`, "{}", 1, true); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Errorf("resample partway: count=%d, want 1", n)
	}

	if err := db.UpsertPendingLogGroup(nsID, fullApp, "h", "d", `["f1","f2"]`, "{}", 2, false); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Errorf("resample complete, awaiting review: count=%d, want 1", n)
	}

	if err := configureLogGroup(db, nsID, fullApp, "schema-v2"); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Errorf("reconfigured: count=%d, want 0", n)
	}
}
