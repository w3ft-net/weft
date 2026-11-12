//go:build !defaultshape

package shellfs

import (
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted"
)

// newHosted is New under the hosted tenancy, which the tests of this package run as.
func newHosted(s *store.Store, org store.OrgID, logServices []string, opts ...Option) *ShellFS {
	return New(s, org, logServices, append([]Option{WithTenancy(hosted.NewAdapter(s))}, opts...)...)
}
