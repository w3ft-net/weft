// Package hosted is the org/team Scoper, over the org_permissions and
// namespace_permissions tables. It moves to the private hosted repo in
// docs/plans/tenant-scope.md Phase 4.
package hosted

import (
	"context"
	"errors"
	"fmt"

	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
)

// Actor is a user in an org. An empty User is the system identity (crons and
// internal callers), which reads everything in the org.
type Actor struct {
	Org  store.OrgID
	User string
}

func (a Actor) Partition() store.OrgID { return a.Org }

type Scoper struct{ Store *store.Store }

// ReadScope grants an owner and the system identity every namespace, and
// anyone else the namespaces they hold read permission on.
func (s Scoper) ReadScope(_ context.Context, a tenancy.Actor) (shape.Set, error) {
	act, ok := a.(Actor)
	if !ok {
		return nil, fmt.Errorf("hosted scoper: unsupported actor %T", a)
	}
	if act.Org == 0 {
		return nil, errors.New("hosted scoper: actor has no org")
	}
	owner := act.User == ""
	if !owner {
		var err error
		if owner, err = s.Store.IsOwner(act.Org, act.User); err != nil {
			return nil, fmt.Errorf("hosted scoper: %w", err)
		}
	}
	if owner {
		return shape.Set{{Customer: int64(act.Org)}}, nil
	}
	names, err := s.Store.ReadableNamespaces(act.Org, act.User, false)
	if err != nil {
		return nil, fmt.Errorf("hosted scoper: %w", err)
	}
	set := make(shape.Set, 0, len(names))
	for _, n := range names {
		set = append(set, shape.Coord{Customer: int64(act.Org), Namespace: shape.Value(n)})
	}
	return set, nil
}
