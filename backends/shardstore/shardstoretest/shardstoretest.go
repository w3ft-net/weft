// Package shardstoretest is the conformance suite for shardstore.Store: the
// behaviour every implementation of the shard-state domain (Postgres for the
// hosted build, SQLite for the open one) must show. An implementation's test
// calls Run with a constructor.
//
// The suite holds the contract only: what a caller of the interface may rely
// on, including the facts of an archive export and an upgrade in flight.
package shardstoretest

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"
)

// Subject is one store under test, fresh and empty.
type Subject struct {
	Store shardstore.Store
	// Namespace returns an id shard_services may reference. A store whose
	// namespaces live elsewhere (Postgres) creates one; one that keeps no
	// namespace table may return any value.
	Namespace func(t *testing.T) uint
	// Concurrent says the store takes concurrent callers, so the suite may race
	// export claims against each other. A store scoped to one transaction
	// (the Postgres run) does not.
	Concurrent bool
}

// Run runs the suite, building a fresh Subject for each test.
func Run(t *testing.T, newSubject func(t *testing.T) Subject) {
	cases := []struct {
		name string
		fn   func(*testing.T, Subject)
	}{
		{"ShardRowRoundTrip", shardRowRoundTrip},
		{"NarrowWritesTouchOnlyTheirFields", narrowWrites},
		{"SetShardAddrRefusesAMovedShard", addrGuard},
		{"ListAndDelete", listAndDelete},
		{"LatestPreRecoveryState", latestPreRecoveryState},
		{"StuckArchiving", stuckArchiving},
		{"ShardServices", shardServices},
		{"FieldFacts", fieldFacts},
		{"Heartbeats", heartbeats},
		{"FreshStoreHasTheDefaultBucket", defaultBucket},
		{"Buckets", buckets},
		{"BucketStateAndCascade", bucketStateAndCascade},
	}
	cases = append(cases, factsCases...)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newSubject(t)) })
	}
}

var seq int

func newBucket(t *testing.T, s Subject) shardstore.Bucket {
	t.Helper()
	seq++
	b := shardstore.Bucket{
		Label: fmt.Sprintf("conf-%d-%d", time.Now().UnixNano(), seq), Tier: "small",
		TargetIngestingCount: 2, CreatedAt: "2026-05-21T12:00:00Z", WindowDuration: "1h0m0s",
		State: "STASIS",
	}
	if err := s.Store.SaveBucket(&b); err != nil {
		t.Fatalf("SaveBucket: %v", err)
	}
	return b
}

var t0 = time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)

func newShard(t *testing.T, s Subject, bucketID int64, id string, state shardstore.ShardState) *shardstore.ShardRow {
	t.Helper()
	sh := &shardstore.ShardRow{
		ID: id, State: state, BucketID: bucketID,
		WindowStart: t0, WindowEnd: t0.Add(time.Hour), CreatedAt: t0, Tier: "small",
	}
	if err := s.Store.SaveShard(sh); err != nil {
		t.Fatalf("SaveShard %s: %v", id, err)
	}
	return sh
}

func get(t *testing.T, s Subject, id string) shardstore.ShardRow {
	t.Helper()
	sh, ok, err := s.Store.GetShard(id)
	if err != nil || !ok {
		t.Fatalf("GetShard %s: ok=%v err=%v", id, ok, err)
	}
	return utc(*sh)
}

// utc normalizes the zone a driver reads times back in.
func utc(r shardstore.ShardRow) shardstore.ShardRow {
	r.WindowStart, r.WindowEnd, r.CreatedAt = r.WindowStart.UTC(), r.WindowEnd.UTC(), r.CreatedAt.UTC()
	r.LastHeartbeatAt = r.LastHeartbeatAt.UTC()
	return r
}

func shardRowRoundTrip(t *testing.T, s Subject) {
	b := newBucket(t, s)
	if _, ok, err := s.Store.GetShard("absent"); ok || err != nil {
		t.Fatalf("GetShard of an unknown id: ok=%v err=%v, want not found and no error", ok, err)
	}
	stripe := 3
	want := shardstore.ShardRow{
		ID: "s-1", WindowStart: t0, WindowEnd: t0.Add(time.Hour), State: shardstore.StateIngesting,
		IngestFailures: 2, Addr: "10.0.0.1:9000", InstanceID: "i-1", ParquetPath: "s3://x/y",
		Tier: "medium", RowCount: 42, CreatedAt: t0, EBSVolumeID: "vol-1", HeartbeatToken: "tok",
		BucketID: b.ID, LastHeartbeatAt: t0.Add(time.Minute), AZ: "us-east-2a", StripeIndex: &stripe,
		LaunchAttempts: 4, BinarySHA: "abc", DiskBytes: 1 << 30,
	}
	if err := s.Store.SaveShard(&want); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, "s-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("row did not round trip:\n got %+v\nwant %+v", got, want)
	}

	// Saving again is an upsert, and a nil stripe index stays nil.
	want.State, want.StripeIndex, want.Addr = shardstore.StateReadOnly, nil, ""
	if err := s.Store.SaveShard(&want); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, "s-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("second save:\n got %+v\nwant %+v", got, want)
	}
}

func narrowWrites(t *testing.T, s Subject) {
	b := newBucket(t, s)
	newShard(t, s, b.ID, "s-1", shardstore.StateLaunching)
	st := s.Store
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	before := get(t, s, "s-1")

	must(st.SetShardState("s-1", shardstore.StateIngesting))
	want := before
	want.State = shardstore.StateIngesting
	if got := get(t, s, "s-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("SetShardState touched more than state:\n got %+v\nwant %+v", got, want)
	}

	stripe := 1
	must(st.SetShardLaunchResult("s-1", "i-1", "us-east-2b", "vol-1", &stripe))
	want.InstanceID, want.AZ, want.EBSVolumeID, want.StripeIndex = "i-1", "us-east-2b", "vol-1", &stripe
	if got := get(t, s, "s-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("SetShardLaunchResult:\n got %+v\nwant %+v", got, want)
	}

	must(st.SetShardAddr("s-1", "i-1", "10.0.0.2:9000"))
	want.Addr = "10.0.0.2:9000"
	must(st.SetShardIngestFailures("s-1", 3))
	want.IngestFailures = 3
	must(st.IncrementShardLaunchAttempts("s-1"))
	must(st.IncrementShardLaunchAttempts("s-1"))
	want.LaunchAttempts = 2
	must(st.SetShardParquetPath("s-1", "s3://x"))
	want.ParquetPath = "s3://x"
	if got := get(t, s, "s-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("addr, failures, attempts, parquet path:\n got %+v\nwant %+v", got, want)
	}

	// A zero value writes: a reset must not be mistaken for "no change".
	must(st.SetShardIngestFailures("s-1", 0))
	must(st.ClearShardVolume("s-1"))
	must(st.ClearShardInstance("s-1"))
	want.IngestFailures, want.EBSVolumeID, want.InstanceID = 0, "", ""
	if got := get(t, s, "s-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("clears:\n got %+v\nwant %+v", got, want)
	}

	// A heartbeat writes its four fields; a beat with no disk footprint must
	// not erase the last known one.
	at := t0.Add(time.Hour)
	must(st.TouchHeartbeat("s-1", 100, at, "sha1", 500))
	must(st.TouchHeartbeat("s-1", 0, at.Add(time.Minute), "sha2", 0))
	want.RowCount, want.LastHeartbeatAt, want.BinarySHA, want.DiskBytes = 0, at.Add(time.Minute), "sha2", 500
	if got := get(t, s, "s-1"); !reflect.DeepEqual(got, want) {
		t.Fatalf("TouchHeartbeat:\n got %+v\nwant %+v", got, want)
	}
}

func addrGuard(t *testing.T, s Subject) {
	b := newBucket(t, s)
	newShard(t, s, b.ID, "s-1", shardstore.StateIngesting)
	st := s.Store
	// No instance yet: any beat may name one.
	if err := st.SetShardAddr("s-1", "i-old", "a:1"); err != nil {
		t.Fatalf("first address: %v", err)
	}
	if err := st.SetShardLaunchResult("s-1", "i-new", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetShardAddr("s-1", "i-old", "stale:1"); !errors.Is(err, shardstore.ErrShardMoved) {
		t.Fatalf("a demoted instance's address: %v, want ErrShardMoved", err)
	}
	if got := get(t, s, "s-1"); got.Addr != "a:1" {
		t.Fatalf("a refused write changed the address to %q", got.Addr)
	}
	// A beat that names no instance skips the check.
	if err := st.SetShardAddr("s-1", "", "b:2"); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, "s-1"); got.Addr != "b:2" {
		t.Fatalf("address %q, want b:2", got.Addr)
	}
}

func listAndDelete(t *testing.T, s Subject) {
	b1, b2 := newBucket(t, s), newBucket(t, s)
	for i, id := range []string{"s-a", "s-b", "s-c"} {
		sh := newShard(t, s, b1.ID, id, shardstore.StateIngesting)
		sh.WindowStart = t0.Add(time.Duration(i) * time.Hour)
		if err := s.Store.SaveShard(sh); err != nil {
			t.Fatal(err)
		}
	}
	newShard(t, s, b2.ID, "s-other", shardstore.StateReadOnly)

	all, err := s.Store.ListShards()
	if err != nil || len(all) != 4 {
		t.Fatalf("ListShards: %d err=%v, want 4", len(all), err)
	}
	in1, err := s.Store.ListShardsByBucket(b1.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, sh := range in1 {
		ids = append(ids, sh.ID)
	}
	if !reflect.DeepEqual(ids, []string{"s-c", "s-b", "s-a"}) {
		t.Fatalf("ListShardsByBucket order: %v, want newest window first", ids)
	}

	if err := s.Store.DeleteShard("s-b"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Store.GetShard("s-b"); ok {
		t.Fatal("a deleted shard is still readable")
	}
	if err := s.Store.DeleteShard("s-b"); err != nil {
		t.Fatalf("deleting a missing shard: %v", err)
	}
}

// LatestPreRecoveryState reads the history of state changes, so the store must
// record one however the change is made.
func latestPreRecoveryState(t *testing.T, s Subject) {
	b := newBucket(t, s)
	newShard(t, s, b.ID, "s-1", shardstore.StateIngesting)
	st := s.Store
	got := func() shardstore.ShardState {
		t.Helper()
		v, err := st.LatestPreRecoveryState("s-1")
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := got(); v != "" {
		t.Fatalf("no recovery yet: %q, want empty", v)
	}
	step := func(to shardstore.ShardState) {
		t.Helper()
		if err := st.SetShardState("s-1", to); err != nil {
			t.Fatal(err)
		}
	}
	step(shardstore.StateReadOnly)
	step(shardstore.StateUpgrading)
	step(shardstore.StateLaunching)
	step(shardstore.StateReadOnly)
	if v := got(); v != shardstore.StateReadOnly {
		t.Fatalf("after READ_ONLY→UPGRADING: %q, want READ_ONLY", v)
	}
	step(shardstore.StateIngesting)
	step(shardstore.StateSuspended)
	step(shardstore.StateLaunching)
	step(shardstore.StateIngesting)
	if v := got(); v != shardstore.StateIngesting {
		t.Fatalf("after INGESTING→SUSPENDED: %q, want INGESTING (the latest recovery)", v)
	}

	// A write of the same state is not a transition.
	step(shardstore.StateIngesting)
	if v := got(); v != shardstore.StateIngesting {
		t.Fatalf("a no-op state write changed the history: %q", v)
	}

	// Saving a row whose state changed is a transition too.
	sh, _, _ := st.GetShard("s-1")
	sh.State = shardstore.StateSuspended
	if err := st.SaveShard(sh); err != nil {
		t.Fatal(err)
	}
	sh.State = shardstore.StateIngesting
	if err := st.SaveShard(sh); err != nil {
		t.Fatal(err)
	}
	if v := got(); v != shardstore.StateIngesting {
		t.Fatalf("after a SaveShard into SUSPENDED: %q, want INGESTING", v)
	}
}

func stuckArchiving(t *testing.T, s Subject) {
	b := newBucket(t, s)
	newShard(t, s, b.ID, "s-1", shardstore.StateReadOnly)
	newShard(t, s, b.ID, "s-2", shardstore.StateReadOnly)
	for _, id := range []string{"s-1", "s-2"} {
		if err := s.Store.SetShardState(id, shardstore.StateArchiving); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Store.SetShardState("s-2", shardstore.StateArchived); err != nil {
		t.Fatal(err)
	}

	stuck, err := s.Store.StuckArchiving(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(stuck) != 1 || stuck[0].ShardID != "s-1" {
		t.Fatalf("StuckArchiving: %+v, want only s-1 (s-2 left ARCHIVING)", stuck)
	}
	stuck, err = s.Store.StuckArchiving(time.Now().Add(-time.Hour))
	if err != nil || len(stuck) != 0 {
		t.Fatalf("StuckArchiving before it began: %+v err=%v, want none", stuck, err)
	}
}

func shardServices(t *testing.T, s Subject) {
	b := newBucket(t, s)
	newShard(t, s, b.ID, "s-1", shardstore.StateIngesting)
	newShard(t, s, b.ID, "s-2", shardstore.StateIngesting)
	ns, ns2 := s.Namespace(t), s.Namespace(t)
	st := s.Store

	if err := st.StampShardServices("s-1", nil); err != nil {
		t.Fatalf("stamping nothing: %v", err)
	}
	stamp := []shardstore.ShardService{{NamespaceID: ns, Service: "api"}, {NamespaceID: ns, Service: "web"}}
	for i := 0; i < 2; i++ { // idempotent
		if err := st.StampShardServices("s-1", stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AddShardService("s-2", ns, "api"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddShardService("s-2", ns, "api"); err != nil {
		t.Fatalf("adding a service twice: %v", err)
	}
	if err := st.AddShardService("s-2", ns2, "api"); err != nil {
		t.Fatal(err)
	}

	rows, err := st.ListShardServices()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, r := range rows {
		have[fmt.Sprintf("%s/%d/%s", r.ShardID, r.NamespaceID, r.Service)] = true
	}
	for _, k := range []string{
		fmt.Sprintf("s-1/%d/api", ns), fmt.Sprintf("s-1/%d/web", ns),
		fmt.Sprintf("s-2/%d/api", ns), fmt.Sprintf("s-2/%d/api", ns2),
	} {
		if !have[k] {
			t.Errorf("ListShardServices is missing %s", k)
		}
	}
	if len(rows) != 4 {
		t.Errorf("ListShardServices: %d rows, want 4 (stamping twice must not duplicate)", len(rows))
	}

	ids, err := st.ShardIDsServingService(ns, "web")
	if err != nil || !reflect.DeepEqual(ids, map[string]struct{}{"s-1": {}}) {
		t.Fatalf("serving web: %v err=%v", ids, err)
	}
	ids, _ = st.ShardIDsServingService(ns, "api")
	if !reflect.DeepEqual(ids, map[string]struct{}{"s-1": {}, "s-2": {}}) {
		t.Fatalf("serving api: %v", ids)
	}
	ids, _ = st.ShardIDsServingService(ns, "")
	if len(ids) != 2 {
		t.Fatalf("the namespace wildcard: %v, want both shards", ids)
	}
	ids, _ = st.ShardIDsServingService(ns2, "web")
	if len(ids) != 0 {
		t.Fatalf("an unserved service: %v, want none", ids)
	}
}

func fieldFacts(t *testing.T, s Subject) {
	b := newBucket(t, s)
	newShard(t, s, b.ID, "s-1", shardstore.StateIngesting)
	st := s.Store
	f := func(svc string, epoch int64) shardstore.ShardFieldFact {
		return shardstore.ShardFieldFact{CustomerID: 7, Namespace: "ns", Service: svc, Kind: "main", Env: "prod", Locator: "level", Epoch: epoch}
	}
	if err := st.ApplyShardFieldFacts("s-1", []shardstore.ShardFieldFact{f("a", 1), f("b", 1)}, nil); err != nil {
		t.Fatal(err)
	}
	// An upsert moves the epoch; a dropped fact goes; the row's own ShardID is
	// ignored in favour of the argument.
	upd := f("a", 5)
	upd.ShardID = "ignored"
	if err := st.ApplyShardFieldFacts("s-1", []shardstore.ShardFieldFact{upd}, []shardstore.ShardFieldFact{f("b", 0)}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListShardFieldFacts()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ShardID != "s-1" || rows[0].Service != "a" || rows[0].Epoch != 5 {
		t.Fatalf("field facts: %+v, want one for a at epoch 5", rows)
	}
}

func heartbeats(t *testing.T, s Subject) {
	st := s.Store
	rec := func(shard, inst, addr string, rows int64) {
		t.Helper()
		if err := st.RecordHeartbeat(&shardstore.ShardHeartbeat{ShardID: shard, InstanceID: inst, Addr: addr, State: "INGESTING", RowCount: rows, DiskBytes: 9}); err != nil {
			t.Fatal(err)
		}
	}
	rec("s-1", "i-old", "old:1", 1)
	rec("s-1", "i-old", "old:1", 2) // the same source upserts
	rec("", "i-spare", "spare:1", 0)
	rec("", "i-spare2", "spare:2", 0)

	live, err := st.LiveShardHeartbeats(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]shardstore.ShardHeartbeat{}
	for _, h := range live {
		byKey[h.ShardID+"/"+h.InstanceID] = h
		if h.Age < 0 || h.Age > time.Minute {
			t.Errorf("beat %s/%s has age %v", h.ShardID, h.InstanceID, h.Age)
		}
	}
	if len(live) != 3 || byKey["s-1/i-old"].RowCount != 2 || byKey["s-1/i-old"].Addr != "old:1" || byKey["s-1/i-old"].DiskBytes != 9 {
		t.Fatalf("live beats: %+v, want s-1 once at its latest, plus both unclaimed instances", live)
	}

	if err := st.PruneShardHeartbeats(time.Hour); err != nil {
		t.Fatal(err)
	}
	if live, _ = st.LiveShardHeartbeats(time.Minute); len(live) != 3 {
		t.Fatalf("pruning by a long age removed fresh beats: %d left", len(live))
	}
}

// A store nobody has written to already has the bucket that services with no
// assignment belong to. Without it the coordinator has nothing to launch a
// shard for, and every row is dead-lettered as "no shard serves this service".
func defaultBucket(t *testing.T, s Subject) {
	b, ok, err := s.Store.GetBucketByLabel("default")
	if err != nil || !ok {
		t.Fatalf("a fresh store has no default bucket: ok=%v err=%v", ok, err)
	}
	if b.Tier != "small" || b.TargetIngestingCount != 2 || b.WindowDuration != "168h" {
		t.Errorf("default bucket = %+v, want tier small, 2 ingesting, 168h windows", b)
	}
}

func buckets(t *testing.T, s Subject) {
	st := s.Store
	b := newBucket(t, s)
	if b.ID == 0 {
		t.Fatal("SaveBucket did not assign an id")
	}
	got, ok, err := st.GetBucket(b.ID)
	if err != nil || !ok || got.Label != b.Label || got.TargetIngestingCount != 2 || got.WindowDuration != "1h0m0s" || got.Tier != "small" {
		t.Fatalf("GetBucket: %+v ok=%v err=%v", got, ok, err)
	}
	byLabel, ok, err := st.GetBucketByLabel(b.Label)
	if err != nil || !ok || byLabel.ID != b.ID {
		t.Fatalf("GetBucketByLabel: %+v ok=%v err=%v", byLabel, ok, err)
	}
	if _, ok, err := st.GetBucket(b.ID + 1000); ok || err != nil {
		t.Fatalf("GetBucket of an unknown id: ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.GetBucketByLabel("nope"); ok || err != nil {
		t.Fatalf("GetBucketByLabel of an unknown label: ok=%v err=%v", ok, err)
	}

	b.TargetIngestingCount = 5
	if err := st.SaveBucket(&b); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.GetBucket(b.ID); got.TargetIngestingCount != 5 {
		t.Fatalf("a second save of the same bucket did not update it: %+v", got)
	}
	all, err := st.ListBuckets()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, x := range all {
		if x.ID == b.ID {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("ListBuckets holds the bucket %d times, want once", n)
	}
}

func bucketStateAndCascade(t *testing.T, s Subject) {
	st := s.Store
	b := newBucket(t, s)
	if _, err := st.BucketState(b.ID + 1000); !errors.Is(err, shardstore.ErrUnknownBucket) {
		t.Fatalf("BucketState of an unknown bucket: %v, want ErrUnknownBucket", err)
	}
	if v, err := st.BucketState(b.ID); err != nil || v != shardstore.BucketStasis {
		t.Fatalf("a new bucket's state: %q err=%v, want STASIS", v, err)
	}
	if err := st.SetBucketState(b.ID, shardstore.BucketWarming); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.BucketState(b.ID); v != shardstore.BucketWarming {
		t.Fatalf("state %q, want WARMING", v)
	}

	at := t0.Add(time.Hour)
	if err := st.SetBucketCascadeStart(b.ID, &at); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.GetBucket(b.ID); got.CascadeStartedAt == nil || !got.CascadeStartedAt.Equal(at) {
		t.Fatalf("cascade start: %v, want %v", got.CascadeStartedAt, at)
	}
	if err := st.SetBucketCascadeStart(b.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.GetBucket(b.ID); got.CascadeStartedAt != nil {
		t.Fatalf("cascade start %v after clearing, want none", got.CascadeStartedAt)
	}
	if got, _, _ := st.GetBucket(b.ID); got.State != "WARMING" {
		t.Fatalf("clearing the cascade clock changed the state to %q", got.State)
	}

	newShard(t, s, b.ID, "s-1", shardstore.StateIngesting)
	newShard(t, s, b.ID, "s-2", shardstore.StateReadOnly)
	newShard(t, s, b.ID, "s-3", shardstore.StateArchived)
	other := newBucket(t, s)
	newShard(t, s, other.ID, "s-4", shardstore.StateIngesting)
	in, err := st.ShardsInBucketStates(b.ID, shardstore.StateIngesting, shardstore.StateReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, sh := range in {
		got[sh.ID] = true
	}
	if !reflect.DeepEqual(got, map[string]bool{"s-1": true, "s-2": true}) {
		t.Fatalf("ShardsInBucketStates: %v, want s-1 and s-2", got)
	}
}
