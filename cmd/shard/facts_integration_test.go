package main

import (
	"context"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/coordinator"
	coordsvc "github.com/graybeardcorp/weft/coordinator/service"
	"github.com/graybeardcorp/weft/logdb"
	"github.com/graybeardcorp/weft/logtable"
	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape/shapetest"
	"github.com/graybeardcorp/weft/shard"
	shardsvc "github.com/graybeardcorp/weft/shard/service"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted"
)

// factsRig is a shard server and a coordinator on real Postgres, joined by the
// shard's own heartbeat client.
type factsRig struct {
	t       *testing.T
	db      *store.Store
	prov    *logdb.Provisioner
	reg     *logdb.ShardRegistry
	shard   *shardsvc.Server
	client  pb.WeftCoordinatorClient
	shardID string
	// oldShard makes the heartbeat look as a shard that predates table names
	// sends it: field facts without the table.
	oldShard *atomic.Bool
}

func newFactsRig(t *testing.T) *factsRig {
	t.Helper()
	root, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	db, rollback := root.BeginTest()
	t.Cleanup(func() {
		rollback()
		root.Close()
	})
	def, ok, err := db.GetBucketByLabel("default")
	if err != nil || !ok {
		t.Fatalf("default bucket: ok=%v err=%v", ok, err)
	}

	const shardID, token, fleet = "shard-facts-it", "shard-token", "fleet-secret"

	// The coordinator: a registry over the real store, holding the shard's record.
	reg := logdb.NewShardRegistry()
	reg.SetStore(logdb.ShardStore{Store: db})
	reg.SetLegacyFactTable(hosted.LegacyFactTable)
	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	prov := logdb.NewProvisioner(reg, nil, logdb.ProvisionerConfig{})
	prov.SetStore(db)
	coord.Provisioner = prov
	logdb.SeedShard(prov, &logdb.Shard{
		ID: shardID, State: logdb.StateIngesting, Addr: "10.0.0.1:9000",
		HeartbeatToken: token, BucketID: def.ID,
		WindowStart: time.Now().Add(-time.Hour), WindowEnd: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	})

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	pb.RegisterWeftCoordinatorServer(gs, &coordsvc.Server{Coord: coord, Store: db, ShardSecret: fleet})
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	oldShard := &atomic.Bool{}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			if hb, ok := req.(*pb.ShardHeartbeatRequest); ok && oldShard.Load() {
				for _, d := range hb.FieldFacts {
					d.Table = ""
				}
			}
			return invoker(ctx, method, req, reply, cc, opts...)
		}),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	// The shard: a fresh one on its own volume, the way a new window opens.
	dir := t.TempDir()
	if err := shard.SaveIdentity(dir, &shard.Identity{ShardID: shardID, HeartbeatToken: token}); err != nil {
		t.Fatal(err)
	}
	srv, err := shardsvc.NewServerFromIdentity(dir, filepath.Join(dir, "shard.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shard().Close() })

	return &factsRig{t: t, db: db, prov: prov, reg: reg, shard: srv, client: pb.NewWeftCoordinatorClient(conn), shardID: shardID, oldShard: oldShard}
}

// ingest lands n rows of one source whose $2 and .q are the given values.
func (r *factsRig) ingest(n int, awkField, q string) {
	r.t.Helper()
	rows := make([]*pb.LogRow, n)
	for i := range rows {
		rows[i] = &pb.LogRow{Raw: "x " + awkField, PayloadJson: []byte(`{"q":` + q + `}`),
			TsUnixNs: time.Now().UnixNano(), CustomerId: 1, Namespace: "ns", Service: "svc", Env: "prod"}
	}
	if err := r.shard.Shard().Ingest(context.Background(), rows, nil); err != nil {
		r.t.Fatal(err)
	}
	r.shard.Shard().FlushFieldFacts()
}

// beat sends one heartbeat exactly as the shard binary does.
func (r *factsRig) beat() {
	r.t.Helper()
	if !sendHeartbeat(r.client, "10.0.0.1:9000", "i-it", r.shard, "fleet-secret") {
		r.t.Fatal("heartbeat failed")
	}
}

func (r *factsRig) storedLocators() map[string]bool {
	r.t.Helper()
	rows, err := r.db.ListShardFieldFacts()
	if err != nil {
		r.t.Fatal(err)
	}
	out := map[string]bool{}
	for _, f := range rows {
		if f.ShardID == r.shardID {
			out[f.Locator] = true
		}
	}
	return out
}

func (r *factsRig) typedOnCoordinator(locator string) bool {
	return r.reg.Get(r.shardID).FieldTyped(shard.FactKey{Table: logtable.Name([]any{int64(1)}, "ns", "svc", "main"), Env: "prod", Locator: locator})
}

// A fresh shard types a source from its own rows, reports it on a heartbeat,
// the coordinator keeps it in memory and in Postgres, and the shard stops
// resending once acknowledged. Then a violating row retracts it the same way.
func TestFieldFacts_ShardToCoordinatorToPostgres(t *testing.T) {
	r := newFactsRig(t)

	r.ingest(factTypedAfterForTest, "7", "3")
	// "7" is a number and a whole number: $2 and $2#int (the integer fact a keyed
	// sum needs), and .q.
	if got := r.shard.Shard().FieldFacts(); len(got) != 3 {
		t.Fatalf("shard has %d pending facts before the heartbeat, want $2, $2#int and .q: %+v", len(got), got)
	}
	r.beat()

	for _, loc := range []string{"$2", "$2#int", ".q"} {
		if !r.typedOnCoordinator(loc) {
			t.Errorf("coordinator doesn't hold %s as typed after the heartbeat", loc)
		}
		if !r.storedLocators()[loc] {
			t.Errorf("%s is not in shard_field_facts", loc)
		}
	}
	if got := r.shard.Shard().FieldFacts(); len(got) != 0 {
		t.Errorf("shard still resends %+v after the coordinator acknowledged", got)
	}

	// Another beat with nothing new changes nothing.
	r.beat()
	if len(r.storedLocators()) != 3 {
		t.Errorf("rows after an idle heartbeat: %v", r.storedLocators())
	}

	// A row that isn't a number retracts $2, $2#int and .q everywhere.
	r.ingest(1, "7abc", `"s"`)
	if got := r.shard.Shard().FieldFacts(); len(got) != 3 {
		t.Fatalf("shard has %d pending retractions, want 3: %+v", len(got), got)
	}
	r.beat()
	for _, loc := range []string{"$2", "$2#int", ".q"} {
		if r.typedOnCoordinator(loc) {
			t.Errorf("coordinator still holds %s as typed after the retraction", loc)
		}
	}
	if got := r.storedLocators(); len(got) != 0 {
		t.Errorf("shard_field_facts still has %v after the retraction", got)
	}
	if got := r.shard.Shard().FieldFacts(); len(got) != 0 {
		t.Errorf("shard still resends %+v after the retraction was acknowledged", got)
	}
}

// A fact a shard reports is one the planner's variant can ask for: the key a
// real Plan puts on its TypedPlan is the key the classifier reported, so a
// query for that stream is routed to the shard's typed SQL, and a retraction
// takes it away again.
func TestFieldFacts_ReachTheQueryPlannersKey(t *testing.T) {
	r := newFactsRig(t)
	plan, err := planner.Plan(`cat /log/ns/prod/svc/main.log | awk '$2 > 500'`, shapetest.Tenant(1), "", time.Time{}, time.Time{})
	if err != nil || plan.Typed == nil {
		t.Fatalf("plan: %v, typed %+v", err, plan.Typed)
	}
	tp := plan.Typed
	typed := func() bool {
		return r.prov.ShardFieldsTyped(r.shardID, tp.Table, tp.Env, tp.Requires)
	}

	if typed() {
		t.Fatal("typed before any fact was reported")
	}
	r.ingest(factTypedAfterForTest, "7", "3")
	r.beat()
	if !typed() {
		t.Errorf("the planner's key %+v is not typed after the shard reported it", tp)
	}
	r.ingest(1, "7abc", "3")
	r.beat()
	if typed() {
		t.Error("still typed after the retraction")
	}
}

// The keys a keyed aggregate's plan asks for reach the registry the same way: a
// sum needs the integer fact beside the number one, a max or min only the number
// one, so a field holding fractions types the second plan and not the first.
func TestFieldFacts_KeyedAggregatePlansAskForTheKeysTheShardReports(t *testing.T) {
	typedFor := func(r *factsRig, program string) bool {
		plan, err := planner.Plan(`cat /log/ns/prod/svc/main.log | awk '`+program+`'`, shapetest.Tenant(1), "", time.Time{}, time.Time{})
		if err != nil || plan.Typed == nil {
			t.Fatalf("plan %s: %v, typed %+v", program, err, plan.Typed)
		}
		tp := plan.Typed
		return r.prov.ShardFieldsTyped(r.shardID, tp.Table, tp.Env, tp.Requires)
	}
	const sum = `{ s[$1] += $2 } END { for (k in s) print k, s[k] }`
	const max = `{ if ($2 > m[$1]) m[$1] = $2 } END { for (k in m) print k, m[k] }`

	// Each rig holds a database until its test ends, so each case is a subtest.
	t.Run("whole numbers", func(t *testing.T) {
		r := newFactsRig(t)
		if typedFor(r, sum) || typedFor(r, max) {
			t.Fatal("typed before any fact was reported")
		}
		r.ingest(factTypedAfterForTest, "7", "3")
		r.beat()
		if !typedFor(r, sum) || !typedFor(r, max) {
			t.Error("a field of whole numbers does not type the sum and the max plans")
		}
		r.ingest(1, "7.5", "3")
		r.beat()
		if typedFor(r, sum) {
			t.Error("the sum plan is still typed after a fraction retracted the integer fact")
		}
		if !typedFor(r, max) {
			t.Error("a fraction retracted the number fact the max plan needs: it is still a number")
		}
	})
	t.Run("fractions", func(t *testing.T) {
		r := newFactsRig(t)
		r.ingest(factTypedAfterForTest, "7.5", "3")
		r.beat()
		if typedFor(r, sum) {
			t.Error("the sum plan is typed over a field that never held only whole numbers")
		}
		if !typedFor(r, max) {
			t.Error("the max plan is not typed over a field of numbers")
		}
	})
}

// lossyAck delivers heartbeats but loses the acknowledgement on the way back.
type lossyAck struct{ pb.WeftCoordinatorClient }

func (l lossyAck) Heartbeat(ctx context.Context, in *pb.ShardHeartbeatRequest, opts ...grpc.CallOption) (*pb.HeartbeatResponse, error) {
	if _, err := l.WeftCoordinatorClient.Heartbeat(ctx, in, opts...); err != nil {
		return nil, err
	}
	return &pb.HeartbeatResponse{}, nil
}

// When the acknowledgement is lost the shard sends the same facts again, the
// coordinator takes the duplicate without harm, and the next acknowledgement
// that arrives stops the resending.
func TestFieldFacts_ALostAckIsResentAndTheDuplicateIsHarmless(t *testing.T) {
	r := newFactsRig(t)
	r.ingest(factTypedAfterForTest, "7", "3")
	good := r.client
	r.client = lossyAck{good}

	r.beat()
	if got := r.shard.Shard().FieldFacts(); len(got) != 3 {
		t.Fatalf("after a lost ack the shard has %d pending facts, want 3 to resend", len(got))
	}
	r.beat() // the duplicate
	if got := r.storedLocators(); len(got) != 3 || !r.typedOnCoordinator("$2") {
		t.Errorf("duplicate delivery changed the stored facts: %v", got)
	}

	r.client = good
	r.beat()
	if got := r.shard.Shard().FieldFacts(); len(got) != 0 {
		t.Errorf("still pending %+v after an acknowledgement got through", got)
	}
}

const factTypedAfterForTest = 1000

// A shard of the previous build reports a fact without a table, naming the
// source by customer_id, namespace, service and kind. During an upgrade the
// coordinator must still resolve it to the planner's key, and take the
// retraction the same way.
func TestFieldFacts_AShardWithoutTableNamesStillReachesThePlannersKey(t *testing.T) {
	r := newFactsRig(t)
	r.oldShard.Store(true)
	plan, err := planner.Plan(`cat /log/ns/prod/svc/main.log | awk '$2 > 500'`, shapetest.Tenant(1), "", time.Time{}, time.Time{})
	if err != nil || plan.Typed == nil {
		t.Fatalf("plan: %v, typed %+v", err, plan.Typed)
	}
	tp := plan.Typed
	typed := func() bool { return r.prov.ShardFieldsTyped(r.shardID, tp.Table, tp.Env, tp.Requires) }

	r.ingest(factTypedAfterForTest, "7", "3")
	r.beat()
	if !typed() {
		t.Fatal("a fact from a shard that sends no table is not typed under the planner's table")
	}
	r.ingest(1, "7abc", "3")
	r.beat()
	if typed() {
		t.Error("still typed after the old shard's retraction")
	}
}
