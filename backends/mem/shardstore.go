package mem

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"
)

// Shards is a shardstore.Store in memory: shard rows, buckets, the services
// shards serve, field facts, heartbeats and the export and upgrade facts.
type Shards struct {
	mu       sync.Mutex
	shards   map[string]*shardstore.ShardRow
	ops      []shardOp
	buckets  []*shardstore.Bucket
	nextBkt  int64
	services []shardstore.ShardService
	facts    []shardstore.ShardFieldFact
	beats    map[hbKey]*beat
	now      func() time.Time
}

type shardOp struct {
	shardID  string
	ts       time.Time
	from, to *shardstore.ShardState
}

type hbKey struct{ shard, instance string }

type beat struct {
	hb shardstore.ShardHeartbeat
	at time.Time
}

var _ shardstore.Store = (*Shards)(nil)

// NewShards is a store holding only the default bucket that services with no
// assignment belong to.
func NewShards() *Shards {
	s := &Shards{shards: map[string]*shardstore.ShardRow{}, beats: map[hbKey]*beat{}, now: time.Now}
	s.nextBkt++
	s.buckets = append(s.buckets, &shardstore.Bucket{ID: s.nextBkt, Label: "default", TargetIngestingCount: 2, Tier: "small",
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), WindowDuration: "168h", State: string(shardstore.BucketStasis)})
	return s
}

func (s *Shards) recordOp(id string, from, to *shardstore.ShardState) {
	s.ops = append(s.ops, shardOp{shardID: id, ts: s.now().UTC(), from: from, to: to})
}

func (s *Shards) SaveShard(sh *shardstore.ShardRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, existed := s.shards[sh.ID]
	var oldState shardstore.ShardState
	if existed {
		oldState = old.State
	}
	c := sh.Clone()
	// The export row stays once an attempt was counted: the counter outlives the
	// attempt's other facts, which a zero Export on a later save clears.
	s.shards[sh.ID] = &c
	st := sh.State
	switch {
	case !existed:
		s.recordOp(sh.ID, nil, &st)
	case oldState != sh.State:
		s.recordOp(sh.ID, &oldState, &st)
	}
	return nil
}

func (s *Shards) GetShard(id string) (*shardstore.ShardRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.shards[id]
	if !ok {
		return nil, false, nil
	}
	c := sh.Clone()
	return &c, true, nil
}

func (s *Shards) list(keep func(*shardstore.ShardRow) bool) []*shardstore.ShardRow {
	var out []*shardstore.ShardRow
	for _, sh := range s.shards {
		if keep(sh) {
			c := sh.Clone()
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Shards) ListShards() ([]*shardstore.ShardRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list(func(*shardstore.ShardRow) bool { return true }), nil
}

func (s *Shards) ListShardsByBucket(bucketID int64) ([]*shardstore.ShardRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.list(func(sh *shardstore.ShardRow) bool { return sh.BucketID == bucketID })
	sort.SliceStable(out, func(i, j int) bool { return out[i].WindowStart.After(out[j].WindowStart) })
	return out, nil
}

func (s *Shards) DeleteShard(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.shards[id]
	if !ok {
		return nil
	}
	state := old.State
	delete(s.shards, id)
	kept := s.services[:0]
	for _, sv := range s.services {
		if sv.ShardID != id {
			kept = append(kept, sv)
		}
	}
	s.services = kept
	facts := s.facts[:0]
	for _, f := range s.facts {
		if f.ShardID != id {
			facts = append(facts, f)
		}
	}
	s.facts = facts
	s.recordOp(id, &state, nil)
	return nil
}

// update runs one narrow write on a shard row; a missing row is not an error.
func (s *Shards) update(id string, f func(*shardstore.ShardRow)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sh, ok := s.shards[id]; ok {
		f(sh)
	}
}

func (s *Shards) SetShardState(id string, state shardstore.ShardState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.shards[id]
	if !ok {
		return nil
	}
	old := sh.State
	sh.State = state
	if old != state {
		s.recordOp(id, &old, &state)
	}
	return nil
}

func (s *Shards) TouchHeartbeat(id string, rowCount int64, at time.Time, binarySHA string, diskBytes int64) error {
	s.update(id, func(sh *shardstore.ShardRow) {
		sh.RowCount, sh.LastHeartbeatAt, sh.BinarySHA = rowCount, at.UTC(), binarySHA
		if diskBytes > 0 {
			sh.DiskBytes = diskBytes
		}
	})
	return nil
}

func (s *Shards) SetShardLaunchResult(id, instanceID, az, volumeID string, stripeIndex *int) error {
	s.update(id, func(sh *shardstore.ShardRow) {
		sh.InstanceID, sh.AZ, sh.EBSVolumeID = instanceID, az, volumeID
		sh.StripeIndex = nil
		if stripeIndex != nil {
			v := *stripeIndex
			sh.StripeIndex = &v
		}
	})
	return nil
}

func (s *Shards) SetShardAddr(id, instanceID, addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.shards[id]
	if !ok || !(instanceID == "" || sh.InstanceID == instanceID || sh.InstanceID == "") {
		return shardstore.ErrShardMoved
	}
	sh.Addr = addr
	return nil
}

func (s *Shards) SetShardIngestFailures(id string, n int) error {
	s.update(id, func(sh *shardstore.ShardRow) { sh.IngestFailures = n })
	return nil
}

func (s *Shards) IncrementShardLaunchAttempts(id string) error {
	s.update(id, func(sh *shardstore.ShardRow) { sh.LaunchAttempts++ })
	return nil
}

func (s *Shards) SetShardParquetPath(id, path string) error {
	s.update(id, func(sh *shardstore.ShardRow) { sh.ParquetPath = path })
	return nil
}

func (s *Shards) ClearShardVolume(id string) error {
	s.update(id, func(sh *shardstore.ShardRow) { sh.EBSVolumeID = "" })
	return nil
}

func (s *Shards) ClearShardInstance(id string) error {
	s.update(id, func(sh *shardstore.ShardRow) { sh.InstanceID = "" })
	return nil
}

func (s *Shards) LatestPreRecoveryState(id string) (shardstore.ShardState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.ops) - 1; i >= 0; i-- {
		op := s.ops[i]
		if op.shardID == id && op.to != nil && (*op.to == shardstore.StateUpgrading || *op.to == shardstore.StateSuspended) {
			if op.from == nil {
				return "", nil
			}
			return *op.from, nil
		}
	}
	return "", nil
}

func (s *Shards) StuckArchiving(before time.Time) ([]shardstore.StuckShard, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	since := map[string]time.Time{}
	for _, op := range s.ops {
		if op.to != nil && *op.to == shardstore.StateArchiving {
			if sh, ok := s.shards[op.shardID]; ok && sh.State == shardstore.StateArchiving && op.ts.After(since[op.shardID]) {
				since[op.shardID] = op.ts
			}
		}
	}
	var out []shardstore.StuckShard
	for id, t := range since {
		if t.Before(before) {
			out = append(out, shardstore.StuckShard{ShardID: id, Since: t})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out, nil
}

func (s *Shards) hasService(shardID string, ns uint, service string) bool {
	for _, sv := range s.services {
		if sv.ShardID == shardID && sv.NamespaceID == ns && sv.Service == service {
			return true
		}
	}
	return false
}

func (s *Shards) StampShardServices(shardID string, services []shardstore.ShardService) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sv := range services {
		if !s.hasService(shardID, sv.NamespaceID, sv.Service) {
			s.services = append(s.services, shardstore.ShardService{ShardID: shardID, NamespaceID: sv.NamespaceID, Service: sv.Service})
		}
	}
	return nil
}

func (s *Shards) AddShardService(shardID string, namespaceID uint, service string) error {
	return s.StampShardServices(shardID, []shardstore.ShardService{{NamespaceID: namespaceID, Service: service}})
}

func (s *Shards) ListShardServices() ([]shardstore.ShardService, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]shardstore.ShardService(nil), s.services...), nil
}

func (s *Shards) ShardIDsServingService(namespaceID uint, service string) (map[string]struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]struct{}{}
	for _, sv := range s.services {
		if sv.NamespaceID == namespaceID && (service == "" || sv.Service == service) {
			out[sv.ShardID] = struct{}{}
		}
	}
	return out, nil
}

func (s *Shards) ListShardFieldFacts() ([]shardstore.ShardFieldFact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]shardstore.ShardFieldFact(nil), s.facts...), nil
}

// sameFact is the primary key of a field fact: everything but the epoch.
func sameFact(a, b shardstore.ShardFieldFact) bool {
	a.Epoch, b.Epoch = 0, 0
	return a == b
}

func (s *Shards) ApplyShardFieldFacts(shardID string, typed, dropped []shardstore.ShardFieldFact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range typed {
		f.ShardID = shardID
		found := false
		for i := range s.facts {
			if sameFact(s.facts[i], f) {
				s.facts[i].Epoch, found = f.Epoch, true
			}
		}
		if !found {
			s.facts = append(s.facts, f)
		}
	}
	for _, f := range dropped {
		f.ShardID = shardID
		kept := s.facts[:0]
		for _, x := range s.facts {
			if !sameFact(x, f) {
				kept = append(kept, x)
			}
		}
		s.facts = kept
	}
	return nil
}

func (s *Shards) RecordHeartbeat(hb *shardstore.ShardHeartbeat) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beats[hbKey{hb.ShardID, hb.InstanceID}] = &beat{hb: *hb, at: s.now().UTC()}
	return nil
}

func (s *Shards) LiveShardHeartbeats(within time.Duration) ([]shardstore.ShardHeartbeat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var live []*beat
	for _, b := range s.beats {
		if b.at.After(now.Add(-within)) {
			live = append(live, b)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].at.After(live[j].at) })
	var out []shardstore.ShardHeartbeat
	claimed := map[string]bool{}
	for _, b := range live {
		h := b.hb
		// Newest first: a claimed shard keeps its newest beat only, so a replaced
		// instance's old beat cannot resurface its dead address.
		if h.ShardID != "" {
			if claimed[h.ShardID] {
				continue
			}
			claimed[h.ShardID] = true
		}
		h.Age = now.Sub(b.at)
		out = append(out, h)
	}
	return out, nil
}

func (s *Shards) PruneShardHeartbeats(maxAge time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-maxAge)
	for k, b := range s.beats {
		if b.at.Before(cutoff) {
			delete(s.beats, k)
		}
	}
	return nil
}

func (s *Shards) ListBuckets() ([]shardstore.Bucket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []shardstore.Bucket
	for _, b := range s.buckets {
		out = append(out, cpBucket(b))
	}
	return out, nil
}

func cpBucket(b *shardstore.Bucket) shardstore.Bucket {
	c := *b
	if b.CascadeStartedAt != nil {
		t := *b.CascadeStartedAt
		c.CascadeStartedAt = &t
	}
	return c
}

func (s *Shards) bucket(f func(*shardstore.Bucket) bool) *shardstore.Bucket {
	for _, b := range s.buckets {
		if f(b) {
			return b
		}
	}
	return nil
}

func (s *Shards) GetBucket(id int64) (shardstore.Bucket, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.bucket(func(b *shardstore.Bucket) bool { return b.ID == id }); b != nil {
		return cpBucket(b), true, nil
	}
	return shardstore.Bucket{}, false, nil
}

func (s *Shards) GetBucketByLabel(label string) (shardstore.Bucket, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.bucket(func(b *shardstore.Bucket) bool { return b.Label == label }); b != nil {
		return cpBucket(b), true, nil
	}
	return shardstore.Bucket{}, false, nil
}

func (s *Shards) SaveBucket(b *shardstore.Bucket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := b.State
	if state == "" {
		state = string(shardstore.BucketStasis)
	}
	for _, x := range s.buckets {
		if x.Label == b.Label && x.ID != b.ID {
			return fmt.Errorf("bucket label %q already exists", b.Label)
		}
	}
	c := cpBucket(b)
	c.State = state
	if b.ID == 0 {
		s.nextBkt++
		b.ID, c.ID = s.nextBkt, s.nextBkt
		s.buckets = append(s.buckets, &c)
		return nil
	}
	if x := s.bucket(func(x *shardstore.Bucket) bool { return x.ID == b.ID }); x != nil {
		*x = c
		return nil
	}
	s.buckets = append(s.buckets, &c)
	if b.ID > s.nextBkt {
		s.nextBkt = b.ID
	}
	return nil
}

func (s *Shards) BucketState(id int64) (shardstore.BucketState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.bucket(func(b *shardstore.Bucket) bool { return b.ID == id })
	if b == nil {
		return "", shardstore.ErrUnknownBucket
	}
	if b.State == "" {
		return shardstore.BucketStasis, nil
	}
	return shardstore.BucketState(b.State), nil
}

func (s *Shards) SetBucketState(id int64, st shardstore.BucketState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.bucket(func(b *shardstore.Bucket) bool { return b.ID == id }); b != nil {
		b.State = string(st)
	}
	return nil
}

func (s *Shards) SetBucketCascadeStart(id int64, at *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.bucket(func(b *shardstore.Bucket) bool { return b.ID == id }); b != nil {
		b.CascadeStartedAt = nil
		if at != nil {
			t := at.UTC()
			b.CascadeStartedAt = &t
		}
	}
	return nil
}

func (s *Shards) ShardsInBucketStates(bucketID int64, states ...shardstore.ShardState) ([]*shardstore.ShardRow, error) {
	if len(states) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list(func(sh *shardstore.ShardRow) bool {
		if sh.BucketID != bucketID {
			return false
		}
		for _, st := range states {
			if sh.State == st {
				return true
			}
		}
		return false
	}), nil
}

// --- export and upgrade facts ---

func (s *Shards) BeginExportAttempt(id string, prevAttempt int, instanceType string, startedAt time.Time, maxActive int) (shardstore.ExportClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.shards[id]
	if !ok {
		return shardstore.ExportClaimLost, nil
	}
	if sh.Export.Attempt != prevAttempt {
		return shardstore.ExportClaimLost, nil
	}
	active := 0
	for oid, o := range s.shards {
		if oid != id && o.Export.StartedAt != nil {
			active++
		}
	}
	if active >= maxActive {
		return shardstore.ExportClaimHeld, nil
	}
	t := startedAt.UTC()
	sh.Export.InstanceType, sh.Export.StartedAt, sh.Export.Attempt = instanceType, &t, prevAttempt+1
	return shardstore.ExportClaimWon, nil
}

func (s *Shards) SetExportSnapshot(id, snapshotID string) error {
	s.update(id, func(sh *shardstore.ShardRow) { sh.Export.SnapshotID = snapshotID })
	return nil
}

func (s *Shards) SetExportInstance(id, instanceID string) error {
	s.update(id, func(sh *shardstore.ShardRow) { sh.Export.Instance = instanceID })
	return nil
}

func (s *Shards) ClearExportFacts(id string) error {
	s.update(id, func(sh *shardstore.ShardRow) {
		sh.Export = shardstore.ExportFacts{Attempt: sh.Export.Attempt}
	})
	return nil
}

func (s *Shards) LatestShardDiskBytes(id string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sh, ok := s.shards[id]; ok {
		return sh.DiskBytes, nil
	}
	return 0, nil
}

func (s *Shards) SetShardUpgrade(id string, from shardstore.UpgradePhase, f shardstore.UpgradeFacts) error {
	moved := fmt.Errorf("%w: %s is no longer in phase %q", shardstore.ErrUpgradeMoved, id, from)
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.shards[id]
	if !ok {
		return moved
	}
	have := sh.Upgrade
	exists := have != (shardstore.UpgradeFacts{})
	if from == "" {
		if f == (shardstore.UpgradeFacts{}) {
			return nil
		}
		if exists {
			return moved
		}
		sh.Upgrade = f.Clone()
		return nil
	}
	if !exists || have.Phase != from {
		return moved
	}
	sh.Upgrade = f.Clone()
	return nil
}
