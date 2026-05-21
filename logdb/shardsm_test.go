package logdb

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kevlarcon/weft/store"
)

// fakeShardSMStore is the in-memory ShardSMStore used by SM tests.
// Stores one ShardRecord per id, mutating it through the SM's
// external-storage accessors. Tracks delete calls so cleanup paths
// can assert. Concurrent-safe — the SM may call from multiple
// goroutines if FiringQueued ever surfaces one.
type fakeShardSMStore struct {
	mu        sync.Mutex
	records   map[string]*store.ShardRecord
	deletions []string
}

func newFakeShardSMStore() *fakeShardSMStore {
	return &fakeShardSMStore{records: map[string]*store.ShardRecord{}}
}

func (f *fakeShardSMStore) put(rec *store.ShardRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *rec
	f.records[rec.ID] = &cp
}

func (f *fakeShardSMStore) GetShard(id string) (store.ShardRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[id]
	if !ok {
		return store.ShardRecord{}, false, nil
	}
	return *rec, true, nil
}

func (f *fakeShardSMStore) UpdateShardState(id, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[id]
	if !ok {
		return nil
	}
	rec.State = state
	return nil
}

func (f *fakeShardSMStore) TouchLastHeartbeatAt(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[id]
	if !ok {
		return nil
	}
	now := time.Now().UTC()
	rec.LastHeartbeatAt = &now
	return nil
}

func (f *fakeShardSMStore) DeleteShard(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.records, id)
	f.deletions = append(f.deletions, id)
	return nil
}

// makeFixtureShardSM builds a ShardSM in initialState with a fresh
// entry + fake store. Each test starts from this baseline and drives
// the SM forward via Fire.
func makeFixtureShardSM(t *testing.T, initialState ShardState) (*ShardSM, *fakeShardSMStore, *ShardEntry) {
	t.Helper()
	fstore := newFakeShardSMStore()
	fstore.put(&store.ShardRecord{
		ID:       "s-test",
		State:    string(initialState),
		BucketID: 1,
		Tier:     "small",
	})
	entry := &ShardEntry{
		ID:       "s-test",
		BucketID: 1,
		Tier:     "small",
		State:    initialState,
	}
	clock := func() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) }
	sm := NewShardSM(entry, ShardSMDeps{
		Store:          fstore,
		Clock:          clock,
		StaleThreshold: 2 * time.Minute,
	})
	return sm, fstore, entry
}

func TestShardSM_LaunchingToIngestingOnMatchingHeartbeat(t *testing.T) {
	sm, fstore, entry := makeFixtureShardSM(t, StateLaunching)
	entry.InstanceID = "i-abc" // simulated post-launch tag
	fstore.put(&store.ShardRecord{
		ID: "s-test", State: string(StateLaunching), BucketID: 1, Tier: "small",
		InstanceID: "i-abc",
	})

	hb := store.ShardHeartbeat{
		ShardID: "s-test", Addr: "10.0.0.1:9000", InstanceID: "i-abc", RowCount: 7,
	}
	if err := sm.Fire(context.Background(), TriggerHeartbeat, hb); err != nil {
		t.Fatalf("Fire heartbeat: %v", err)
	}

	got, err := sm.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != StateIngesting {
		t.Fatalf("state: got %q want INGESTING", got)
	}
	if entry.Addr != "10.0.0.1:9000" {
		t.Fatalf("entry.Addr: got %q", entry.Addr)
	}
	if entry.RowCount != 7 {
		t.Fatalf("entry.RowCount: got %d", entry.RowCount)
	}
}

func TestShardSM_LaunchingRejectsStrayInstanceHeartbeat(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateLaunching)
	entry.InstanceID = "i-abc"

	hb := store.ShardHeartbeat{
		ShardID: "s-test", Addr: "10.0.0.1:9000", InstanceID: "i-xyz", // wrong instance
	}
	err := sm.Fire(context.Background(), TriggerHeartbeat, hb)
	// stateless returns an error when no permit fires.
	if err == nil {
		t.Fatalf("expected guard rejection; entry stayed at %q", entry.State)
	}
	if entry.State != StateLaunching {
		t.Fatalf("state: got %q want LAUNCHING", entry.State)
	}
}

func TestShardSM_LaunchingToGoneOnLaunchFailed(t *testing.T) {
	sm, fstore, _ := makeFixtureShardSM(t, StateLaunching)
	if err := sm.Fire(context.Background(), TriggerLaunchFailed); err != nil {
		t.Fatalf("Fire LaunchFailed: %v", err)
	}
	// GONE.OnEntry deletes the row → state read returns GONE
	got, err := sm.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != StateGone {
		t.Fatalf("state: got %q want GONE", got)
	}
	if len(fstore.deletions) != 1 || fstore.deletions[0] != "s-test" {
		t.Fatalf("deletions: %v", fstore.deletions)
	}
}

func TestShardSM_IngestingToReadOnlyOnRotate(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateIngesting)
	if err := sm.Fire(context.Background(), TriggerRotateRequested); err != nil {
		t.Fatalf("Fire Rotate: %v", err)
	}
	if entry.State != StateReadOnly {
		t.Fatalf("state: got %q want READ_ONLY", entry.State)
	}
}

func TestShardSM_ReadOnlyToArchived(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateReadOnly)
	if err := sm.Fire(context.Background(), TriggerArchiveRequested); err != nil {
		t.Fatalf("Fire Archive: %v", err)
	}
	if entry.State != StateArchived {
		t.Fatalf("state: got %q want ARCHIVED", entry.State)
	}
}

func TestShardSM_IngestingToSuspendedOnStaleWithVolume(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateIngesting)
	entry.EBSVolumeID = "vol-abc"
	entry.InstanceID = "i-old"
	entry.Addr = "10.0.0.1:9000"
	entry.LastHeartbeatAt = time.Date(2026, 5, 21, 11, 50, 0, 0, time.UTC) // 10min stale

	if err := sm.Fire(context.Background(), TriggerHeartbeatStale); err != nil {
		t.Fatalf("Fire stale: %v", err)
	}
	if entry.State != StateSuspended {
		t.Fatalf("state: got %q want SUSPENDED", entry.State)
	}
	if entry.InstanceID != "" || entry.Addr != "" {
		t.Fatalf("SUSPENDED.OnEntry should have cleared InstanceID/Addr; got %q/%q",
			entry.InstanceID, entry.Addr)
	}
}

func TestShardSM_IngestingToGoneOnStaleWithoutVolume(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateIngesting)
	entry.EBSVolumeID = ""
	entry.LastHeartbeatAt = time.Date(2026, 5, 21, 11, 50, 0, 0, time.UTC)

	if err := sm.Fire(context.Background(), TriggerHeartbeatStale); err != nil {
		t.Fatalf("Fire stale: %v", err)
	}
	got, err := sm.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != StateGone {
		t.Fatalf("state: got %q want GONE", got)
	}
}

func TestShardSM_StaleGuardRejectsWhenFresh(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateIngesting)
	entry.EBSVolumeID = "vol-abc"
	// LastHeartbeatAt 30s ago (well under 2min threshold)
	entry.LastHeartbeatAt = time.Date(2026, 5, 21, 11, 59, 30, 0, time.UTC)

	err := sm.Fire(context.Background(), TriggerHeartbeatStale)
	if err == nil {
		t.Fatalf("expected guard rejection on fresh heartbeat")
	}
	if entry.State != StateIngesting {
		t.Fatalf("state: got %q want unchanged INGESTING", entry.State)
	}
}

func TestShardSM_SuspendedToIngestingOnRebindHeartbeat(t *testing.T) {
	sm, _, entry := makeFixtureShardSM(t, StateSuspended)
	entry.EBSVolumeID = "vol-abc"
	entry.InstanceID = "i-new" // replacement just attached

	hb := store.ShardHeartbeat{
		ShardID: "s-test", Addr: "10.0.0.7:9000", InstanceID: "i-new",
	}
	if err := sm.Fire(context.Background(), TriggerHeartbeat, hb); err != nil {
		t.Fatalf("Fire heartbeat: %v", err)
	}
	if entry.State != StateIngesting {
		t.Fatalf("state: got %q want INGESTING", entry.State)
	}
	if entry.Addr != "10.0.0.7:9000" {
		t.Fatalf("entry.Addr: got %q", entry.Addr)
	}
}

func TestShardSM_SuspendedToGoneOnStranded(t *testing.T) {
	sm, _, _ := makeFixtureShardSM(t, StateSuspended)
	if err := sm.Fire(context.Background(), TriggerStranded); err != nil {
		t.Fatalf("Fire Stranded: %v", err)
	}
	got, err := sm.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != StateGone {
		t.Fatalf("state: got %q want GONE", got)
	}
}

func TestShardSM_ArchivedToGoneOnAdminDelete(t *testing.T) {
	sm, fstore, _ := makeFixtureShardSM(t, StateArchived)
	if err := sm.Fire(context.Background(), TriggerAdminDelete); err != nil {
		t.Fatalf("Fire AdminDelete: %v", err)
	}
	if len(fstore.deletions) != 1 {
		t.Fatalf("expected one deletion, got %v", fstore.deletions)
	}
}

func TestShardSM_ArchivedRejectsRotateAndArchive(t *testing.T) {
	// ARCHIVED is terminal-for-data; only AdminDelete leaves it.
	for _, trigger := range []Trigger{TriggerRotateRequested, TriggerArchiveRequested, TriggerHeartbeatStale, TriggerLaunchFailed} {
		t.Run(string(trigger), func(t *testing.T) {
			sm, _, entry := makeFixtureShardSM(t, StateArchived)
			err := sm.Fire(context.Background(), trigger)
			if err == nil {
				t.Fatalf("expected %q to be rejected from ARCHIVED", trigger)
			}
			if entry.State != StateArchived {
				t.Fatalf("state: got %q want ARCHIVED unchanged", entry.State)
			}
		})
	}
}
