package coordinator

import (
	"context"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/w3ft-net/weft/backends/dlq"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

// deadLetter keeps a batch the drain could not place, and returns where it went.
// It returns "" when there is no store or the write failed, and the batch is
// then lost: the failure is logged and recorded without a path.
func (c *Coordinator) deadLetter(ctx context.Context, added shape.Added, rows []*pb.LogRow) string {
	if c.DLQ == nil || len(rows) == 0 {
		return ""
	}
	ref, err := c.DLQ.Write(ctx, added, rows)
	if err != nil {
		weftsdk.Errorf(ctx, "dlq: failed to keep %d rows: %v", len(rows), err)
		return ""
	}
	weftsdk.Infof(ctx, "dlq: kept %d rows at %s", len(rows), ref)
	return ref
}

// replayDeadLetters feeds every kept batch back into the ingest path.
func (c *Coordinator) replayDeadLetters(ctx context.Context) (int, error) {
	return c.DLQ.Replay(ctx, func(entries []dlq.Entry) {
		rows := make([]Row, len(entries))
		for i, e := range entries {
			rows[i] = Row{e.LogRow, e.Added}
		}
		c.Ingest.Write(rows)
	})
}
