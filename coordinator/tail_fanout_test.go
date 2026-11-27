package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/w3ft-net/weft/shape"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// TestTailFanout_LocalPassthrough confirms that with no Redis configured,
// TailFanout behaves identically to calling the wrapped TailBroker
// directly — the local-only dev/test path this codebase relies on
// everywhere else must be unaffected by TailFanout's existence.
func TestTailFanout_LocalPassthrough(t *testing.T) {
	broker := NewTailBroker()
	f := NewTailFanout(broker, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	matcher := &RowMatcher{Coords: shape.Set{{Namespace: shape.Value("vimgym"), Service: shape.Value("api"), Kind: shape.Value("main")}}}
	id, ch := f.Subscribe(ctx, matcher)
	defer f.Unsubscribe(context.Background(), id, matcher)

	f.Publish([]Row{{LogRow: &pb.LogRow{
		Namespace: "vimgym", Service: "api", Kind: "main", Raw: "local row",
	}}})

	select {
	case row := <-ch:
		if row.Raw != "local row" {
			t.Fatalf("row.Raw = %q, want %q", row.Raw, "local row")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for locally-published row")
	}
}
