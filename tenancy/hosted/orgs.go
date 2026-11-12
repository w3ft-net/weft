package hosted

import (
	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
)

// Orgs is hosted weft's coordinator.Orgs: the hosted shape's one added column
// is the org id, so the two are the same number.
type Orgs struct{}

func (Orgs) OrgOf(a shape.Added) store.OrgID     { return store.OrgID(a.Customer) }
func (Orgs) AddedOf(org store.OrgID) shape.Added { return shape.Added{Customer: int64(org)} }
func (Orgs) ActorOf(org store.OrgID, user string) tenancy.Actor {
	return Actor{Org: org, User: user}
}

// NewAdapter is the hosted tenancy over st, for the shell filesystem and the
// git history to act under.
func NewAdapter(st *store.Store) tenancy.Adapter {
	return tenancy.Adapter{
		ActorOf: Orgs{}.ActorOf,
		Authz:   Authorizer{Store: st},
		Scope:   Scoper{Store: st},
	}
}
