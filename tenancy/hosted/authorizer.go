package hosted

import (
	"context"
	"errors"
	"fmt"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
	"gorm.io/gorm"
)

// Actions of the hosted product, outside the tenancy contract: another
// adapter denies them as unknown.
const (
	ActionManageTeam    tenancy.Action = "hosted/manage-team"    // invite, remove, set member permissions
	ActionManageBilling tenancy.Action = "hosted/manage-billing" // plan, payment method
	ActionSuperadmin    tenancy.Action = "hosted/superadmin"     // platform operator surface
)

// Authorizer decides actions from the owner role, org_permissions,
// namespace_permissions and the superadmin flag. It reads the store on every
// call, so a revoked grant takes effect on the next request.
type Authorizer struct{ Store *store.Store }

func (z Authorizer) Authorize(_ context.Context, a tenancy.Actor, act tenancy.Action, r tenancy.Resource) error {
	actor, ok := a.(Actor)
	if !ok {
		return fmt.Errorf("hosted authorizer: unsupported actor %T", a)
	}
	if actor.User == "" {
		return tenancy.Denied("no user")
	}
	if act == ActionSuperadmin {
		u, err := z.Store.GetUserByID(actor.User)
		if err != nil || !u.IsSuperadmin {
			return tenancy.Denied("forbidden")
		}
		return nil
	}
	if actor.Org == 0 {
		return tenancy.Denied("no org")
	}
	owner, err := z.Store.IsOwner(actor.Org, actor.User)
	if err != nil {
		return fmt.Errorf("hosted authorizer: %w", err)
	}

	switch act {
	case ActionManageTeam, ActionManageBilling, tenancy.ActionDeleteNamespace, tenancy.ActionPublishOrgWide:
		if owner {
			return nil
		}
		return tenancy.Denied("owner only")
	case tenancy.ActionCreateNamespace:
		return z.orgBit(actor, "cannot create namespaces", func(p *store.OrgPermission) bool { return p.CanCreateNamespaces })
	case tenancy.ActionManageBin:
		return z.orgBit(actor, "writing /bin needs the binmgr group", func(p *store.OrgPermission) bool { return p.CanManageFunctions })
	case tenancy.ActionManageEtc:
		return z.orgBit(actor, "writing /etc needs the configmgr group", func(p *store.OrgPermission) bool { return p.CanManageEtc })
	case tenancy.ActionManageCron:
		// A cron's authority is its home namespace, so the manager must be
		// able to read it. Unreadable and nonexistent look the same.
		if r.Namespace == "" {
			return tenancy.Denied("a cron belongs to a namespace")
		}
		if err := z.orgBit(actor, "writing /cron.d needs the cronmgr group", func(p *store.OrgPermission) bool { return p.CanManageWatches }); err != nil {
			return err
		}
		if owner {
			return nil
		}
		ns, err := z.Store.GetNamespace(actor.Org, r.Namespace)
		if err != nil {
			return errCronNamespace
		}
		if perm, err := z.Store.GetNamespacePermission(actor.User, ns.ID); err != nil || !perm.CanRead {
			return errCronNamespace
		}
		return nil
	case tenancy.ActionManageNamespaceTokens:
		if owner {
			return nil
		}
		// A namespace the caller can't see reads the same as one they may
		// not touch, so the answer never says which exists.
		ns, err := z.Store.GetNamespace(actor.Org, r.Namespace)
		if err != nil {
			return errNamespaceForbidden
		}
		perm, err := z.Store.GetNamespacePermission(actor.User, ns.ID)
		if err != nil || !perm.CanCreateTokens {
			return errNamespaceForbidden
		}
		return nil
	}
	return tenancy.Denied("unknown action")
}

var errCronNamespace = tenancy.Denied("no permission to manage crons in this namespace")

var errNamespaceForbidden = tenancy.Denied("no permission to manage tokens and sources in this namespace")

// orgBit allows an action held as one bit of the actor's org_permissions row.
// An owner's right comes from the same row as anyone else's: promotion
// materializes it. No row is read-only.
func (z Authorizer) orgBit(actor Actor, msg string, bit func(*store.OrgPermission) bool) error {
	perm, err := z.Store.GetOrgPermission(actor.Org, actor.User)
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && !bit(perm)) {
		return tenancy.Denied(msg)
	}
	if err != nil {
		return fmt.Errorf("hosted authorizer: %w", err)
	}
	return nil
}
