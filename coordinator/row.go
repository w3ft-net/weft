package coordinator

import (
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

// Row is a log row inside the coordinator: the LogRow as received, and the
// columns the shape adds to it, which the ingest token decided and which are
// never part of the LogRow.
//
// LogRow.customer_id (tag 26) is still stamped on the way in, but only for
// peers that predate the layout: a shard or a coordinator of the previous
// build reads it. Nothing in the coordinator does; it reads Added.
type Row struct {
	*pb.LogRow
	shape.Added
}

// RowsOf wraps rows that share one set of added columns, as a stream's do: the
// token that admitted them fixed those values.
func RowsOf(added shape.Added, rows []*pb.LogRow) []Row {
	out := make([]Row, len(rows))
	for i, r := range rows {
		out[i] = Row{r, added}
	}
	return out
}

// key is the coordinates a subscription's tuples are matched against.
func (r Row) key() shape.Key {
	return shape.Key{Namespace: r.Namespace, Env: r.Env, Service: r.Service, Kind: r.Kind}.InPartition(r.Added.Coord())
}
