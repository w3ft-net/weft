package store

import (
	"context"
	"fmt"
	"strings"

	weftsdk "github.com/w3ft-net/weft-go"
	"gopkg.in/yaml.v3"
)

// EnsureSystemAliases gives every org that has an owner but no system alias
// one that emails the owner, leaving its other aliases untouched. It runs at
// startup so orgs that predate the rule are brought in line, and again for a
// single org whenever it gains an owner. Idempotent.
func (s *Store) EnsureSystemAliases() error {
	return s.ensureSystemAliases(nil)
}

// ensureSystemAliases is EnsureSystemAliases for every org, or for one.
func (s *Store) ensureSystemAliases(only *OrgID) error {
	// An org's owner is whoever was first made its admin; only that member is
	// consulted, since the product creates exactly one.
	q := `SELECT DISTINCT ON (org_id) org_id, user_id, email
	        FROM memberships
	       WHERE role = ? AND status = 'active'
	         AND COALESCE(user_id, '') <> '' AND COALESCE(email, '') <> ''`
	args := []any{string(RoleOwner)}
	if only != nil {
		q += ` AND org_id = ?`
		args = append(args, *only)
	}
	q += ` ORDER BY org_id, created_at, id`

	var owners []struct {
		OrgID  OrgID
		UserID string
		Email  string
	}
	if err := s.db.Raw(q, args...).Scan(&owners).Error; err != nil {
		return err
	}
	for _, o := range owners {
		if err := s.ensureSystemAlias(o.OrgID, o.UserID, o.Email); err != nil {
			return fmt.Errorf("org %d: %w", o.OrgID, err)
		}
	}
	return nil
}

func (s *Store) ensureSystemAlias(orgID OrgID, ownerID, ownerEmail string) error {
	current := ""
	if nc, err := s.GetNotifyConfig(orgID); err == nil {
		current = nc.AliasesYAML
	}
	aliases, err := ParseNotifyAliases(current)
	if err != nil {
		// A config that doesn't parse is the tenant's to fix; leaving it be
		// beats blocking startup over it.
		weftsdk.Errorf(context.Background(), "store: org %d: not adding a system alias to an unreadable notify.yaml: %v", orgID, err)
		return nil
	}
	if aliases[SystemAlias] != nil {
		return nil
	}
	block, err := yaml.Marshal(AliasMap{SystemAlias: {Email: []EmailTarget{{Address: ownerEmail}}}})
	if err != nil {
		return err
	}
	next := string(block)
	if strings.TrimSpace(current) != "" {
		next = strings.TrimRight(current, "\n") + "\n\n" + next
	}
	if err := s.SaveNotifyAliases(orgID, ownerID, next); err != nil {
		// Two processes starting together can both get here; if the other one
		// won, the alias exists and there is nothing left to do.
		if nc, gerr := s.GetNotifyConfig(orgID); gerr == nil {
			if m, perr := ParseNotifyAliases(nc.AliasesYAML); perr == nil && m[SystemAlias] != nil {
				return nil
			}
		}
		return err
	}
	return nil
}
