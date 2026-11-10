// Package tenancy is the seam between weft's query path and whoever decides
// what a principal may see. See docs/plans/tenant-scope.md.
package tenancy

import (
	"context"

	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/store"
)

// Actor is a user acting within a partition. The Scoper that issued it is the
// only code that looks inside.
type Actor interface {
	// Partition is the org whose data the actor is working in.
	Partition() store.OrgID
}

// Scoper decides data visibility.
type Scoper interface {
	// ReadScope is what the actor may read right now. It is called per
	// request and never cached across requests. Any error means deny: there
	// is no fallback scope.
	ReadScope(ctx context.Context, a Actor) (shape.Set, error)
}
