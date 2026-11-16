package compute

import (
	"fmt"

	"github.com/w3ft-net/weft/backends/logs/archive"
	"github.com/w3ft-net/weft/backends/logs/dialect"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// Resolver is the compute-side abstraction over "how do I materialize
// a shard.Shard handle for this shard id." Production has two impls
// (LiveResolver, ColdResolver), tests add a third (InProcessResolver
// in package computetest). Compute's job is to call Dial and run
// QueryToSink — it doesn't know or care whether the resulting handle
// is a gRPC client, a Lambda-backed cold reader, or a direct pointer
// to an in-process DuckDB shard.
//
// Resolvers are constructed by the caller (coordinator side for live
// queries, the remote compute worker for ExecuteQueryRequest off the
// wire). The pool caches the Dial result keyed by ShardID so repeat
// queries against the same shard don't re-open the gRPC connection.
type Resolver interface {
	// ShardID returns the registry id for this shard. Used as the
	// cache key by Engine.
	ShardID() string

	// Dial materializes the shard.Shard handle. Called once per
	// (shard, addr) combination; the pool caches the result. May
	// open gRPC connections, build SigV4 signers, or simply return
	// a pre-built handle depending on the impl.
	Dial() (shard.Shard, error)
}

// LiveResolver dials a hot shard over gRPC. The bearer secret is the
// SHARD_SECRET that the shard-side interceptor checks against; empty
// is only valid in tests where the shard runs without interceptors.
type LiveResolver struct {
	ID     string
	Addr   string
	Secret string
}

func (r *LiveResolver) ShardID() string { return r.ID }

func (r *LiveResolver) Dial() (shard.Shard, error) {
	return shard.NewGRPCShard(r.ID, r.Addr, r.Secret)
}

// ArchiveResolver opens an archived shard through the reader the archive's
// format is routed to. The coordinator planned the archive's SQL in the
// deployment's archive dialect, named in Dialect; a reader that runs another
// would fail the query, so it is refused here, loudly, and not by the engine
// halfway through.
type ArchiveResolver struct {
	Archive archive.Archive
	// Dialect is the dialect the archive's SQL is written in; empty is DuckDB,
	// which is what a coordinator from before it was named plans in.
	Dialect string
	Router  *archive.Router
}

func (r *ArchiveResolver) ShardID() string { return r.Archive.ID }

func (r *ArchiveResolver) Dial() (shard.Shard, error) {
	id := r.Archive.ID
	if r.Router == nil {
		return nil, fmt.Errorf("shard %s is archived, and compute has no archive reader configured", id)
	}
	rd, err := r.Router.Reader(archive.Manifest{Path: r.Archive.Path})
	if err != nil {
		return nil, fmt.Errorf("shard %s: %w", id, err)
	}
	want := r.Dialect
	if want == "" {
		want = dialect.DuckDB.Name()
	}
	if got := rd.Dialect().Name(); got != want {
		return nil, fmt.Errorf("shard %s: its archive tier is written in %s, but its reader runs %s", id, want, got)
	}
	h, err := rd.Open(r.Archive)
	if err != nil {
		return nil, fmt.Errorf("shard %s: %w", id, err)
	}
	if sh, ok := h.(shard.Shard); ok {
		return sh, nil // a reader of this process's own kind of shard needs no adapting
	}
	return &archiveShard{id: id, h: h}, nil
}

// ResolverFromDescriptor is the production dispatcher: given a proto
// descriptor, build the right Resolver. A descriptor with a grpc_addr is a live
// shard; one with a parquet_path is an archived shard, read through archives.
// Neither is the in-process case, which must have been registered separately
// via pool.RegisterResolver before the query ran. Compute never invents
// handles; if a descriptor doesn't carry a dialing recipe and nothing's been
// registered for its id, the caller gets a clear error rather than a silent
// zero-row result.
func ResolverFromDescriptor(d *pb.ShardDescriptor, secret string, archives *archive.Router) (Resolver, error) {
	if d.GrpcAddr != "" {
		return &LiveResolver{ID: d.ShardId, Addr: d.GrpcAddr, Secret: secret}, nil
	}
	if d.ParquetPath != "" {
		return &ArchiveResolver{
			Archive: archive.Archive{
				ID:       d.ShardId,
				Services: d.Services,
				Path:     d.ParquetPath,
				Layout:   shard.LayoutFromProto(d.Layout),
				Endpoint: d.LambdaUrl,
			},
			Dialect: d.Dialect,
			Router:  archives,
		}, nil
	}
	return nil, fmt.Errorf("shard %s: descriptor carries no grpc_addr or parquet_path and no resolver override registered", d.ShardId)
}
