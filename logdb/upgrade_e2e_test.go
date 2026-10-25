//go:build test

package logdb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	shardsvc "github.com/graybeardcorp/weft/shard/service"
	"github.com/graybeardcorp/weft/store"
)

// fakeEBS is one EBS volume both halves of the test see: the leader
// attaches and detaches it through its VolumeManager face, and each shard
// instance mounts and unmounts it through its own Volume face. The
// volume's contents are a directory both servers use as their data dir;
// "mounted" is which instance's process has it open.
type fakeEBS struct {
	mu         sync.Mutex
	changed    chan struct{}
	attachedTo string
	mountedBy  string
}

func newFakeEBS(attachedTo string) *fakeEBS {
	return &fakeEBS{changed: make(chan struct{}), attachedTo: attachedTo, mountedBy: attachedTo}
}

func (e *fakeEBS) notifyLocked() {
	close(e.changed)
	e.changed = make(chan struct{})
}

// instanceVolume is fakeEBS as one instance's shardsvc.Volume.
type instanceVolume struct {
	ebs      *fakeEBS
	instance string
	delay    time.Duration // mount time once attached (device scan, fsck, mount)
}

func (v instanceVolume) Mount(ctx context.Context) error {
	for {
		v.ebs.mu.Lock()
		if v.ebs.attachedTo == v.instance {
			v.ebs.mountedBy = v.instance
			v.ebs.mu.Unlock()
			time.Sleep(v.delay)
			return nil
		}
		ch := v.ebs.changed
		v.ebs.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (v instanceVolume) Unmount(context.Context) error {
	v.ebs.mu.Lock()
	defer v.ebs.mu.Unlock()
	if v.ebs.mountedBy == v.instance {
		v.ebs.mountedBy = ""
	}
	return nil
}

// ebsVolumes is fakeEBS as the leader's VolumeManager. A non-forced
// detach of a still-mounted volume fails, like a busy filesystem would
// hang the real detach.
type ebsVolumes struct {
	fakeVolumeManager
	ebs     *fakeEBS
	shardID string
}

func (m *ebsVolumes) FindByShard(_ context.Context, shardID string) (*VolumeInfo, error) {
	if shardID != m.shardID {
		return nil, nil
	}
	m.ebs.mu.Lock()
	defer m.ebs.mu.Unlock()
	return &VolumeInfo{VolumeID: "vol-e2e", ShardID: shardID, InstanceID: m.ebs.attachedTo}, nil
}

func (m *ebsVolumes) DetachFromInstance(_ context.Context, _ string, force bool) error {
	m.ebs.mu.Lock()
	defer m.ebs.mu.Unlock()
	if !force && m.ebs.mountedBy != "" {
		return fmt.Errorf("detach: volume still mounted by %s", m.ebs.mountedBy)
	}
	m.ebs.attachedTo, m.ebs.mountedBy = "", ""
	m.ebs.notifyLocked()
	return nil
}

func (m *ebsVolumes) AttachToInstance(_ context.Context, _ string, instanceID, _ string) error {
	m.ebs.mu.Lock()
	defer m.ebs.mu.Unlock()
	if m.ebs.attachedTo != "" && m.ebs.attachedTo != instanceID {
		return errors.New("VolumeInUse")
	}
	m.ebs.attachedTo = instanceID
	m.ebs.notifyLocked()
	return nil
}

func serveShard(t *testing.T, srv *shardsvc.Server) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(srv.UnaryInterceptor()),
		grpc.ChainStreamInterceptor(srv.StreamInterceptor()),
	)
	pb.RegisterShardQueryServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

// A roll under continuous ingest and query load: the shard moves to the
// spare with every acknowledged row present exactly once and no query
// failing or answering short.
func TestUpgradeEndToEnd_ARollUnderLoadLosesNoRowAndFailsNoQuery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const shardID = "s-test"
	now := time.Now().UTC()

	// The volume: identity and database live in volDir, mounted on i-old.
	volDir := t.TempDir()
	if err := shard.SaveIdentity(volDir, &shard.Identity{ShardID: shardID, WindowStart: now.Add(-time.Hour), WindowEnd: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ebs := newFakeEBS("i-old")
	oldSrv, err := shardsvc.NewServerFromIdentity(volDir, filepath.Join(volDir, "shard.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	oldSrv.Volume = instanceVolume{ebs: ebs, instance: "i-old"}
	// The spare takes a while to mount and open the volume once attached,
	// so the load meets the shard mid-move and has to hold.
	spareSrv := shardsvc.NewSpareServer(volDir, filepath.Join(volDir, "shard.duckdb"), instanceVolume{ebs: ebs, instance: "i-spare", delay: 500 * time.Millisecond})
	go spareSrv.Activate(ctx)
	t.Cleanup(func() {
		for _, s := range []*shardsvc.Server{oldSrv, spareSrv} {
			if ds := s.Shard(); ds != nil {
				ds.Close()
			}
		}
	})
	oldAddr, spareAddr := serveShard(t, oldSrv), serveShard(t, spareSrv)

	// The leader: the real FSM with the real gRPC configurer and pauser.
	ec2 := newFakeSpareEC2("ami-1")
	reg := NewShardRegistry()
	var pool *SparePool
	sh, db, _ := makeFixtureShardSM(t, StateIngesting, func(d *ShardDeps) {
		d.Clock = time.Now
		d.Registry = reg
		d.Volumes = &ebsVolumes{ebs: ebs, shardID: shardID}
		d.Configurer = grpcConfigurer{}
		d.Pauser = grpcPauser{}
		d.Launcher = &fakeLauncher{}
		d.UpgradeRouteGrace = 200 * time.Millisecond
		pool = NewSparePool(d.Store.(ShardStore), ec2, time.Now)
		d.Spares = pool
	})
	ss := ShardStore{db}
	if err := ss.SetShardLaunchResult(shardID, "i-old", "us-east-2a", "vol-e2e", nil); err != nil {
		t.Fatal(err)
	}
	if err := ss.SetShardAddr(shardID, oldInstance, oldAddr); err != nil {
		t.Fatal(err)
	}
	live, err := grpcConfigurer{}.Dial(ctx, oldAddr, &pb.ConfigureRequest{ShardId: shardID})
	if err != nil {
		t.Fatal(err)
	}
	sh.mu.Lock()
	sh.InstanceID, sh.Addr, sh.AZ, sh.EBSVolumeID = "i-old", oldAddr, "us-east-2a", "vol-e2e"
	sh.WindowStart, sh.WindowEnd = now.Add(-time.Hour), now.Add(time.Hour)
	sh.LastHeartbeatAt = time.Now()
	sh.Services = []store.ShardService{{ShardID: shardID, NamespaceID: 1, Service: "svc"}}
	sh.Live = live
	sh.mu.Unlock()
	reg.mu.Lock()
	reg.entries[shardID] = sh
	reg.mu.Unlock()

	// The pool: the successor, and the replacement that covers the pair
	// once the successor is claimed.
	for i, id := range []string{"i-spare", "i-spare-next"} {
		addr := spareAddr
		if id != "i-spare" {
			addr = "10.9.9.9:9000"
		}
		if err := ss.InsertSpare(&Spare{InstanceID: id, Tier: "small", AZ: "us-east-2a", ImageID: "ami-1", LaunchedAt: now.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
		if err := ss.ObserveSpareHeartbeat(id, addr, "sha-new", time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	// Load: one writer acknowledging rows in sequence, two readers checking
	// that every answer holds at least every row acknowledged before it
	// asked. Readers resolve the shard the way compute does: a handle per
	// (shard, addr) from the routing descriptors.
	var (
		acked     atomic.Int64
		failures  = make(chan error, 100)
		loadWG    sync.WaitGroup
		queries   atomic.Int64
		stopLoad  = make(chan struct{})
		handlesMu sync.Mutex
		handles   = map[string]*shard.GRPCShard{}
	)
	fail := func(err error) {
		select {
		case failures <- err:
		default:
		}
	}
	loadWG.Add(1)
	go func() {
		defer loadWG.Done()
		for seq := int64(1); ; seq++ {
			select {
			case <-stopLoad:
				return
			default:
			}
			h := reg.IngestingShard(1, "svc")
			if h == nil {
				fail(fmt.Errorf("row %d: no INGESTING shard routable", seq))
				return
			}
			row := &pb.LogRow{Raw: fmt.Sprintf("row-%d", seq), Service: "svc", TsUnixNs: time.Now().UnixNano()}
			if err := h.Ingest(ctx, []*pb.LogRow{row}, nil); err != nil {
				fail(fmt.Errorf("row %d: %w", seq, err))
				return
			}
			acked.Store(seq)
			time.Sleep(5 * time.Millisecond)
		}
	}()
	for r := 0; r < 2; r++ {
		loadWG.Add(1)
		go func() {
			defer loadWG.Done()
			for {
				select {
				case <-stopLoad:
					return
				default:
				}
				floor := acked.Load()
				entries, err := reg.MatchingEntries(TimeRange{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}, nil, store.Tier("pro"))
				if err != nil {
					fail(fmt.Errorf("routing: %w", err))
					return
				}
				for _, e := range entries {
					e.mu.RLock()
					id, addr := e.ID, e.Addr
					e.mu.RUnlock()
					handlesMu.Lock()
					h := handles[id+"@"+addr]
					if h == nil {
						if h, err = shard.NewGRPCShard(id, addr, ""); err != nil {
							handlesMu.Unlock()
							fail(err)
							return
						}
						handles[id+"@"+addr] = h
					}
					handlesMu.Unlock()
					sink := &countingSink{}
					h.QueryToSink(ctx, "SELECT raw, ts FROM logs", nil, sink)
					if sink.err != nil {
						fail(fmt.Errorf("query: %w", sink.err))
						return
					}
					if int64(sink.rows) < floor {
						fail(fmt.Errorf("query answered %d rows with %d already acknowledged", sink.rows, floor))
						return
					}
				}
				queries.Add(1)
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}

	time.Sleep(200 * time.Millisecond) // load running against the old instance
	if err := sh.startUpgrade(ctx); err != nil {
		t.Fatalf("startUpgrade: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := sh.Evaluate(ctx); err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		st, _ := sh.smState(ctx)
		if st == StateIngesting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("upgrade stuck in %s/%s", st, sh.UpgradePhase)
		}
		select {
		case err := <-failures:
			t.Fatalf("load failed mid-upgrade (phase %s): %v", sh.UpgradePhase, err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	time.Sleep(200 * time.Millisecond) // load running against the new instance
	close(stopLoad)
	loadWG.Wait()
	select {
	case err := <-failures:
		t.Fatalf("load failed: %v", err)
	default:
	}
	handlesMu.Lock()
	for _, h := range handles {
		h.Close()
	}
	handlesMu.Unlock()

	if sh.Addr != spareAddr || sh.InstanceID != "i-spare" {
		t.Fatalf("shard on %s at %s after the roll, want i-spare at %s", sh.InstanceID, sh.Addr, spareAddr)
	}
	ds := spareSrv.Shard()
	if ds == nil {
		t.Fatal("the spare never opened the shard")
	}
	var n, distinct int64
	rows, err := ds.QueryContext(ctx, "SELECT count(*)::VARCHAR AS raw, now() AS ts FROM logs")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Sscan(rows[0].Raw, &n)
	rows, err = ds.QueryContext(ctx, "SELECT count(DISTINCT raw)::VARCHAR AS raw, now() AS ts FROM logs")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Sscan(rows[0].Raw, &distinct)
	if n != acked.Load() || distinct != n {
		t.Fatalf("after the roll: %d rows (%d distinct), %d acknowledged — want every acknowledged row exactly once", n, distinct, acked.Load())
	}
	if queries.Load() == 0 {
		t.Fatal("no queries ran")
	}
	t.Logf("rolled under load: %d rows acknowledged, %d query rounds", acked.Load(), queries.Load())
}

type countingSink struct {
	mu   sync.Mutex
	rows int
	err  error
}

func (s *countingSink) SendRow(*pb.ResultRow, string) { s.mu.Lock(); s.rows++; s.mu.Unlock() }
func (s *countingSink) Fail(_ string, err error)      { s.mu.Lock(); s.err = err; s.mu.Unlock() }
func (s *countingSink) SendSentinel(string)           {}
