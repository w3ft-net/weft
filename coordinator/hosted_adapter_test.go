//go:build !defaultshape

package coordinator

import (
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/hosted"
)

// The tests of this package run as hosted weft: a Coordinator built without an
// adapter gets the hosted one. (tenancy/hosted/hostedtest does this for other
// packages' tests; it cannot be imported here, since it imports this package.)
func init() {
	DefaultAdapter = Adapter{
		Orgs:       hosted.Orgs{},
		Scoper:     func(st *store.Store) tenancy.Scoper { return hosted.Scoper{Store: st} },
		Authorizer: func(st *store.Store) tenancy.Authorizer { return hosted.Authorizer{Store: st} },
	}
}
