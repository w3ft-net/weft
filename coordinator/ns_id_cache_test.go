//go:build !defaultshape

package coordinator

import (
	"context"
	"github.com/graybeardcorp/weft/shape/shapetest"
	"testing"
	"time"
)

// TestIngestNamespaceID_DeletedNamespaceIsNotPinned: a namespace deleted and
// re-created under the same name gets a new id; ingest routing must follow it
// rather than keep the old one.
func TestIngestNamespaceID_DeletedNamespaceIsNotPinned(t *testing.T) {
	prev := nsIDTTL
	nsIDTTL = 10 * time.Millisecond
	t.Cleanup(func() { nsIDTTL = prev })
	db, org := testStoreAndCustomer(t)
	c := &Coordinator{Store: db}

	old, err := db.FindOrCreateNamespace(org, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ingestNamespaceID(shapetest.Added(int64(org)), "alpha"); got != old.ID {
		t.Fatalf("first resolve = %d, want %d", got, old.ID)
	}
	if err := db.DeleteNamespace(context.Background(), org, "alpha"); err != nil {
		t.Fatal(err)
	}
	fresh, err := db.FindOrCreateNamespace(org, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == old.ID {
		t.Skip("namespace id reused")
	}
	time.Sleep(2 * nsIDTTL)
	if got := c.ingestNamespaceID(shapetest.Added(int64(org)), "alpha"); got != fresh.ID {
		t.Fatalf("after recreate resolve = %d, want %d (stale id %d)", got, fresh.ID, old.ID)
	}
}
