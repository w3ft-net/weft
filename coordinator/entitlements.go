package coordinator

import (
	"time"

	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/tenancy"
)

// unlimited is the Entitlements of a build with no plans: nobody is limited.
type unlimited struct{}

func (unlimited) LimitsFor(shape.Added) tenancy.Limits     { return tenancy.Limits{} }
func (unlimited) LimitsOfPlan(string) tenancy.Limits       { return tenancy.Limits{} }
func (unlimited) CronRunTimeout(shape.Added) time.Duration { return 0 }

// entitlements is what this coordinator holds its tenants to.
func (c *Coordinator) entitlements() tenancy.Entitlements {
	if c.Entitlements != nil {
		return c.Entitlements
	}
	return unlimited{}
}

// limits is the limits of the tenant the owner names. An owner that names none
// is held to the lowest plan's limits.
func (c *Coordinator) limits(owner shape.Added) tenancy.Limits {
	if NoTenant(owner) {
		return c.entitlements().LimitsOfPlan("")
	}
	return c.entitlements().LimitsFor(owner)
}
