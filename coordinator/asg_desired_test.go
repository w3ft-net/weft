package coordinator

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeTierScaler records SetDesiredCapacity calls by tier.
// UsesLaunchingRows=true so isLaunchingLauncher treats it as async.
type fakeTierScaler struct {
	mu        sync.Mutex
	maxByTier map[string]int32
	calls     []tierDesiredCall
}

type tierDesiredCall struct {
	tier    string
	desired int32
}

func (f *fakeTierScaler) LaunchInstance(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (f *fakeTierScaler) TerminateInstance(_ context.Context, _, _ string) error { return nil }
func (f *fakeTierScaler) UsesLaunchingRows() bool                                { return true }

func (f *fakeTierScaler) SetDesiredCapacity(_ context.Context, tier string, desired int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, tierDesiredCall{tier: tier, desired: desired})
	return nil
}

func (f *fakeTierScaler) TierMaxSize(_ context.Context, tier string) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if max, ok := f.maxByTier[tier]; ok {
		return max, nil
	}
	return 100, nil // test default: effectively unlimited
}

func (f *fakeTierScaler) callFor(tier string) (int32, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.tier == tier {
			return c.desired, true
		}
	}
	return 0, false
}

// bucketsWithTargets returns a stubBuckets-equivalent that holds
// multiple buckets with explicit tiers. Needed by the desired-sync
// tests that span more than one tier; single-bucket tests can keep
// using bucketStub.
type manyBucketsStub struct {
	byLabel map[string]*AppBucket
}

func (m *manyBucketsStub) Default() (*AppBucket, error) {
	return m.byLabel["default"], nil
}
func (m *manyBucketsStub) ByID(id int64) (*AppBucket, error) {
	for _, b := range m.byLabel {
		if b.ID == id {
			return b, nil
		}
	}
	return nil, nil
}
func (m *manyBucketsStub) ByLabel(label string) (*AppBucket, error) {
	return m.byLabel[label], nil
}
func (m *manyBucketsStub) All() ([]*AppBucket, error) {
	out := make([]*AppBucket, 0, len(m.byLabel))
	for _, b := range m.byLabel {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (m *manyBucketsStub) EvaluateSplit(_ map[string]int64) (string, error) { return "", nil }
func (m *manyBucketsStub) Split(_ string) (*AppBucket, error)               { return nil, nil }
func (m *manyBucketsStub) SetStore(_ BucketStore)                           {}
func (m *manyBucketsStub) AddAppToBucket(_ int64, _ string) (*AppBucket, error) {
	return nil, fmt.Errorf("not supported in stub")
}

// ──────────────────────────────────────────────────────────

// The coordinator must set absolute desired_capacity from the sum
// of bucket targets (by tier) plus the count of READ_ONLY registry
// entries in the same tier. LAUNCHING + INGESTING are *implied* by
// target and must not be double-counted.
func TestSyncASGDesired_SumsTargetsPlusReadOnlyPerTier(t *testing.T) {
	reg := NewShardRegistry()
	// Two READ_ONLY shards on the small tier (past-window, still hot).
	reg.Add(&ShardEntry{ID: "ro-1", BucketID: 1, Tier: "small", State: StateReadOnly})
	reg.Add(&ShardEntry{ID: "ro-2", BucketID: 1, Tier: "small", State: StateReadOnly})

	// Buckets: small has two buckets (targets 2 and 3); medium has one (target 1).
	buckets := &manyBucketsStub{byLabel: map[string]*AppBucket{
		"default":     {ID: 1, Label: "default", Target: 2, Tier: "small"},
		"dedicated-a": {ID: 2, Label: "dedicated-a", Target: 3, Tier: "small"},
		"medium-b":    {ID: 3, Label: "medium-b", Target: 1, Tier: "medium"},
	}}

	scaler := &fakeTierScaler{}
	prov := &Provisioner{
		registry: reg,
		launcher: scaler,
		Buckets:  buckets,
	}

	prov.syncASGDesiredCapacity(context.Background())

	// small: targets 2+3 = 5, plus 2 READ_ONLY = 7.
	if got, ok := scaler.callFor("small"); !ok {
		t.Errorf("no SetDesiredCapacity call for small")
	} else if got != 7 {
		t.Errorf("small desired: got %d, want 7 (targets 2+3 + readonly 2)", got)
	}
	// medium: target 1, no READ_ONLY → 1.
	if got, ok := scaler.callFor("medium"); !ok {
		t.Errorf("no SetDesiredCapacity call for medium")
	} else if got != 1 {
		t.Errorf("medium desired: got %d, want 1", got)
	}
}

// Only READ_ONLY counts from the registry — INGESTING and LAUNCHING
// are covered by target. An INGESTING shard in the registry must
// not inflate desired beyond target.
func TestSyncASGDesired_IngestingNotDoubleCounted(t *testing.T) {
	reg := NewShardRegistry()
	// One INGESTING shard in the bucket. It should NOT contribute
	// extra capacity — the target already accounts for it.
	reg.Add(&ShardEntry{ID: "ing-1", BucketID: 1, Tier: "small", State: StateIngesting})

	buckets := &manyBucketsStub{byLabel: map[string]*AppBucket{
		"default": {ID: 1, Label: "default", Target: 2, Tier: "small"},
	}}

	scaler := &fakeTierScaler{}
	prov := &Provisioner{
		registry: reg,
		launcher: scaler,
		Buckets:  buckets,
	}

	prov.syncASGDesiredCapacity(context.Background())

	if got, ok := scaler.callFor("small"); !ok {
		t.Errorf("no SetDesiredCapacity call for small")
	} else if got != 2 {
		t.Errorf("small desired: got %d, want 2 (target only; INGESTING already covered)", got)
	}
}

// ARCHIVED shards are excluded — their instances will be terminated
// by §4 and shouldn't count against desired.
func TestSyncASGDesired_ExcludesArchived(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{ID: "arch-1", BucketID: 1, Tier: "small", State: StateArchived})

	buckets := &manyBucketsStub{byLabel: map[string]*AppBucket{
		"default": {ID: 1, Label: "default", Target: 1, Tier: "small"},
	}}

	scaler := &fakeTierScaler{}
	prov := &Provisioner{
		registry: reg,
		launcher: scaler,
		Buckets:  buckets,
	}

	prov.syncASGDesiredCapacity(context.Background())

	if got, ok := scaler.callFor("small"); !ok {
		t.Errorf("no SetDesiredCapacity call for small")
	} else if got != 1 {
		t.Errorf("small desired: got %d, want 1 (ARCHIVED excluded)", got)
	}
}

// Computed desired is clamped to the ASG's MaxSize; a clamp should
// log a tripwire. We assert the clamp here; the log is best-effort.
func TestSyncASGDesired_ClampsToMaxSize(t *testing.T) {
	reg := NewShardRegistry()
	for i := 0; i < 3; i++ {
		reg.Add(&ShardEntry{ID: fmt.Sprintf("ro-%d", i), BucketID: 1, Tier: "small", State: StateReadOnly})
	}

	buckets := &manyBucketsStub{byLabel: map[string]*AppBucket{
		"default": {ID: 1, Label: "default", Target: 10, Tier: "small"},
	}}

	scaler := &fakeTierScaler{
		maxByTier: map[string]int32{"small": 8},
	}
	prov := &Provisioner{
		registry: reg,
		launcher: scaler,
		Buckets:  buckets,
	}

	prov.syncASGDesiredCapacity(context.Background())

	// Computed: target 10 + 3 RO = 13. Max: 8. Expect clamp.
	if got, ok := scaler.callFor("small"); !ok {
		t.Errorf("no SetDesiredCapacity call for small")
	} else if got != 8 {
		t.Errorf("small desired: got %d, want 8 (clamped to MaxSize)", got)
	}
}

// Sync launchers (no TierScaler) must not call anything — they
// manage their own lifecycle. Regression: a stray SetDesiredCapacity
// call against a nil/unsupported scaler would panic.
func TestSyncASGDesired_NoOpForSyncLauncher(t *testing.T) {
	reg := NewShardRegistry()
	buckets := &manyBucketsStub{byLabel: map[string]*AppBucket{
		"default": {ID: 1, Label: "default", Target: 2, Tier: "small"},
	}}

	// LocalLauncher doesn't implement TierScaler.
	launcher := NewLocalLauncher()
	t.Cleanup(func() { launcher.Close() })

	prov := &Provisioner{
		registry: reg,
		launcher: launcher,
		Buckets:  buckets,
	}

	// Must not panic.
	prov.syncASGDesiredCapacity(context.Background())
}

// READ_ONLY shards only count against their own tier. A READ_ONLY
// shard on 'medium' must not inflate the 'small' count.
func TestSyncASGDesired_ReadOnlyFiltersByTier(t *testing.T) {
	reg := NewShardRegistry()
	reg.Add(&ShardEntry{ID: "ro-medium", BucketID: 2, Tier: "medium", State: StateReadOnly})

	buckets := &manyBucketsStub{byLabel: map[string]*AppBucket{
		"small-default":  {ID: 1, Label: "small-default", Target: 2, Tier: "small"},
		"medium-default": {ID: 2, Label: "medium-default", Target: 1, Tier: "medium"},
	}}

	scaler := &fakeTierScaler{}
	prov := &Provisioner{
		registry: reg,
		launcher: scaler,
		Buckets:  buckets,
	}

	prov.syncASGDesiredCapacity(context.Background())

	if got, ok := scaler.callFor("small"); !ok {
		t.Errorf("no SetDesiredCapacity call for small")
	} else if got != 2 {
		t.Errorf("small desired: got %d, want 2 (medium RO must not leak in)", got)
	}
	if got, ok := scaler.callFor("medium"); !ok {
		t.Errorf("no SetDesiredCapacity call for medium")
	} else if got != 2 {
		t.Errorf("medium desired: got %d, want 2 (target 1 + 1 RO)", got)
	}
}

// Silence unused import warning for time; tests may reference it later.
var _ = time.Second
