package coordinator

import (
	"time"

	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/shape"
)

// unlimited is the Entitlements of a build with no plans: nobody is limited.
type unlimited struct{}

func (unlimited) LimitsFor(shape.Added) identity.Limits    { return identity.Limits{} }
func (unlimited) LimitsOfPlan(string) identity.Limits      { return identity.Limits{} }
func (unlimited) CronRunTimeout(shape.Added) time.Duration { return 0 }

// entitlements is what this coordinator holds its groups to.
func (c *Coordinator) entitlements() identity.Entitlements {
	if c.Entitlements != nil {
		return c.Entitlements
	}
	return unlimited{}
}

// limits is the limits of the group the owner names. An owner that names none
// is held to the lowest plan's limits.
func (c *Coordinator) limits(owner shape.Added) identity.Limits {
	if NoGroup(owner) {
		return c.entitlements().LimitsOfPlan("")
	}
	return c.entitlements().LimitsFor(owner)
}
