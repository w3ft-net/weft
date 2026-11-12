// Package hostedtest installs the hosted adapter as coordinator.DefaultAdapter.
// A test binary of a hosted build imports it for its effect, so the coordinators
// and servers it builds without an explicit adapter act as hosted weft does.
// Production binaries never import it: cmd/coordinator passes its adapter.
package hostedtest

import (
	"github.com/graybeardcorp/weft/coordinator"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/hosted"
)

func init() {
	coordinator.DefaultAdapter = coordinator.Adapter{
		Orgs:       hosted.Orgs{},
		Scoper:     func(st *store.Store) tenancy.Scoper { return hosted.Scoper{Store: st} },
		Authorizer: func(st *store.Store) tenancy.Authorizer { return hosted.Authorizer{Store: st} },
	}
}
