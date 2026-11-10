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
		// With a namespace: changing a script that namespace's crons run, which
		// takes its write group. Without one: a script no cron runs, which any
		// team member may change.
		if r.Namespace != "" {
			if owner {
				return nil
			}
			return z.holds(actor, r.Namespace, func(p *store.NamespacePermission) bool { return p.CanWrite })
		}
		m, err := z.Store.GetTeamMemberByUserID(actor.Org, actor.User)
		if err != nil || m.Pending() {
			return tenancy.Denied("only team members change /bin")
		}
		return nil
	case tenancy.ActionManageEtc:
		return z.orgBit(actor, "writing /etc needs the configmgr group", func(p *store.OrgPermission) bool { return p.CanManageEtc })
	case tenancy.ActionGrantNamespaceRead:
		// You can only hand out what you hold: write on the namespace whose
		// crons gain the access, and read on the one they gain it to.
		if owner {
			return nil
		}
		if err := z.holds(actor, r.Namespace, func(p *store.NamespacePermission) bool { return p.CanWrite }); err != nil {
			return err
		}
		return z.holds(actor, r.Target, func(p *store.NamespacePermission) bool { return p.CanRead })
	case tenancy.ActionManageCron, tenancy.ActionManageNamespaceTokens:
		// A namespace's write group manages its crons, tokens and sources. A
		// namespace the caller can't write reads the same as one that doesn't
		// exist, so the answer never says which.
		if act == tenancy.ActionManageCron && r.Namespace == "" {
			return tenancy.Denied("a cron belongs to a namespace")
		}
		if owner {
			return nil
		}
		return z.holds(actor, r.Namespace, func(p *store.NamespacePermission) bool { return p.CanWrite })
	}
	return tenancy.Denied("unknown action")
}

// holds reports whether the actor has the permission on a namespace. A
// namespace that doesn't exist reads the same as one the actor lacks it on.
func (z Authorizer) holds(actor Actor, namespace string, has func(*store.NamespacePermission) bool) error {
	ns, err := z.Store.GetNamespace(actor.Org, namespace)
	if err != nil {
		return errNamespaceForbidden
	}
	perm, err := z.Store.GetNamespacePermission(actor.User, ns.ID)
	if err != nil || !has(perm) {
		return errNamespaceForbidden
	}
	return nil
}

var errNamespaceForbidden = tenancy.Denied("you need this namespace's write group (<namespace>-w) to manage its crons, tokens and sources")

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
