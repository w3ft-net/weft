package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

// TeamPermsDoc is the parsed form of /etc/team.yaml — one entry per
// non-owner team member, listing their org-scoped flags and per-
// namespace permissions. Owners are not represented: they have
// implicit full access, and including them would invite the YAML
// reader into thinking they could revoke their own access.
//
// The map is keyed by email so members can be reordered without
// changing the canonical serialization order.
type TeamPermsDoc map[string]MemberPerms

// MemberPerms is one row in the YAML — the per-directory access for
// /bin /cron.d /etc plus per-namespace permissions. Each *Access is
// "edit" or "read-only"; YAML omits the namespaces map when empty.
type MemberPerms struct {
	Bin        AccessLevel             `yaml:"bin" json:"bin"`
	Cron       AccessLevel             `yaml:"cron" json:"cron"`
	Etc        AccessLevel             `yaml:"etc" json:"etc"`
	Namespaces map[string]NamespaceAcl `yaml:"namespaces,omitempty" json:"namespaces,omitempty"`
}

// AccessLevel is a typed string with two valid values, "edit" and
// "read-only", mapping onto the CanManage<X> booleans.
type AccessLevel string

const (
	AccessEdit     AccessLevel = "edit"
	AccessReadOnly AccessLevel = "read-only"
)

// CanManage reports whether this level allows mutation.
func (a AccessLevel) CanManage() bool { return a == AccessEdit }

// UnmarshalYAML accepts only the two canonical strings and rejects
// anything else — typos like "readonly" or "write" should fail loudly.
func (a *AccessLevel) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("access level must be a string")
	}
	switch v := AccessLevel(node.Value); v {
	case AccessEdit, AccessReadOnly:
		*a = v
		return nil
	default:
		return fmt.Errorf("invalid access level %q (want %q or %q)", node.Value, AccessEdit, AccessReadOnly)
	}
}

// NamespaceAcl is the per-namespace permission row. Both fields are
// boolean; missing == false.
type NamespaceAcl struct {
	Read         bool `yaml:"read" json:"read"`
	CreateTokens bool `yaml:"create_tokens,omitempty" json:"create_tokens,omitempty"`
}

// ParseTeamPerms decodes /etc/team.yaml content. Returns a non-nil
// (possibly empty) doc on success. Empty input is valid — it means
// "no members have any perms".
func ParseTeamPerms(content string) (TeamPermsDoc, error) {
	doc := TeamPermsDoc{}
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return doc, nil
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return doc, nil
}

// Marshal renders the doc as canonical YAML — members sorted by
// email, namespaces sorted by name. Round-trip stable.
func (d TeamPermsDoc) Marshal() ([]byte, error) {
	emails := make([]string, 0, len(d))
	for k := range d {
		emails = append(emails, k)
	}
	sort.Strings(emails)

	var b strings.Builder
	for i, email := range emails {
		if i > 0 {
			b.WriteByte('\n')
		}
		m := d[email]
		fmt.Fprintf(&b, "%s:\n", email)
		fmt.Fprintf(&b, "  bin: %s\n", levelOrDefault(m.Bin))
		fmt.Fprintf(&b, "  cron: %s\n", levelOrDefault(m.Cron))
		fmt.Fprintf(&b, "  etc: %s\n", levelOrDefault(m.Etc))
		if len(m.Namespaces) == 0 {
			b.WriteString("  namespaces: {}\n")
			continue
		}
		b.WriteString("  namespaces:\n")
		nsNames := make([]string, 0, len(m.Namespaces))
		for k := range m.Namespaces {
			nsNames = append(nsNames, k)
		}
		sort.Strings(nsNames)
		for _, name := range nsNames {
			acl := m.Namespaces[name]
			fmt.Fprintf(&b, "    %s:\n", name)
			fmt.Fprintf(&b, "      read: %t\n", acl.Read)
			if acl.CreateTokens {
				b.WriteString("      create_tokens: true\n")
			}
		}
	}
	return []byte(b.String()), nil
}

func levelOrDefault(a AccessLevel) AccessLevel {
	if a == "" {
		return AccessReadOnly
	}
	return a
}

// BuildTeamPerms synthesizes the doc from store state — for every
// non-owner active team member of the customer, it joins their
// OrgPermission row (defaults to all read-only) and their
// NamespacePermission rows. Owners are deliberately omitted.
func (s *Store) BuildTeamPerms(customerID uint) (TeamPermsDoc, error) {
	members, err := s.ListTeamMembers(customerID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}

	namespaces, err := s.ListNamespaces(customerID)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	nsByID := make(map[uint]string, len(namespaces))
	for _, n := range namespaces {
		nsByID[n.ID] = n.Name
	}

	doc := TeamPermsDoc{}
	for _, m := range members {
		if m.Role == "owner" || m.UserID == nil {
			continue
		}
		uid := *m.UserID
		mp := MemberPerms{
			Bin:  AccessReadOnly,
			Cron: AccessReadOnly,
			Etc:  AccessReadOnly,
		}
		if op, err := s.GetOrgPermission(customerID, uid); err == nil && op != nil {
			if op.CanManageFunctions {
				mp.Bin = AccessEdit
			}
			if op.CanManageWatches {
				mp.Cron = AccessEdit
			}
			if op.CanManageEtc {
				mp.Etc = AccessEdit
			}
		} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("load org perms for %s: %w", m.Email, err)
		}

		nsPerms, err := s.ListUserNamespacePermissions(uid)
		if err != nil {
			return nil, fmt.Errorf("load ns perms for %s: %w", m.Email, err)
		}
		if len(nsPerms) > 0 {
			mp.Namespaces = make(map[string]NamespaceAcl, len(nsPerms))
			for _, np := range nsPerms {
				name, ok := nsByID[np.NamespaceID]
				if !ok {
					// Stale permission row pointing at a deleted
					// namespace — skip silently rather than emitting
					// an unresolvable namespace name.
					continue
				}
				mp.Namespaces[name] = NamespaceAcl{
					Read:         np.CanRead,
					CreateTokens: np.CanCreateTokens,
				}
			}
		}
		doc[m.Email] = mp
	}
	return doc, nil
}

// ApplyTeamPerms validates the doc against the customer's team
// membership + namespace list, then upserts both org and namespace
// permissions in a single transaction. Missing-from-YAML is taken to
// mean "default-deny" — perms not in the doc are deleted from the
// store. Two-way authoritative: what's in the YAML is what the user
// gets, full stop.
func (s *Store) ApplyTeamPerms(customerID uint, doc TeamPermsDoc) error {
	members, err := s.ListTeamMembers(customerID)
	if err != nil {
		return fmt.Errorf("list members: %w", err)
	}
	memberByEmail := make(map[string]TeamMember, len(members))
	for _, m := range members {
		memberByEmail[m.Email] = m
	}

	namespaces, err := s.ListNamespaces(customerID)
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}
	nsByName := make(map[string]Namespace, len(namespaces))
	for _, n := range namespaces {
		nsByName[n.Name] = n
	}

	// Validate up-front so a bad doc doesn't half-apply.
	for email, mp := range doc {
		m, ok := memberByEmail[email]
		if !ok {
			return fmt.Errorf("unknown team member: %s", email)
		}
		if m.Role == "owner" {
			return fmt.Errorf("%s is an owner — owners have implicit full access and cannot be listed here", email)
		}
		if m.UserID == nil {
			return fmt.Errorf("%s has not accepted the invite yet — permissions can be set after they sign up", email)
		}
		for nsName := range mp.Namespaces {
			if _, ok := nsByName[nsName]; !ok {
				return fmt.Errorf("%s: unknown namespace %q", email, nsName)
			}
		}
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		txStore := &Store{db: tx}
		// Apply per member: upsert OrgPermission, then reconcile
		// NamespacePermission rows (upsert listed, delete unlisted).
		for email, mp := range doc {
			m := memberByEmail[email]
			uid := *m.UserID
			err := txStore.UpsertOrgPermission(&OrgPermission{
				CustomerID:         customerID,
				UserID:             uid,
				CanManageFunctions: mp.Bin.CanManage(),
				CanManageWatches:   mp.Cron.CanManage(),
				CanManageEtc:       mp.Etc.CanManage(),
			})
			if err != nil {
				return err
			}
			if err := reconcileNsPerms(txStore, uid, mp.Namespaces, nsByName); err != nil {
				return fmt.Errorf("%s: %w", email, err)
			}
		}
		// Members in the team but absent from the doc → default-deny:
		// drop their existing OrgPermission + NamespacePermission rows.
		for _, m := range members {
			if m.Role == "owner" || m.UserID == nil {
				continue
			}
			if _, listed := doc[m.Email]; listed {
				continue
			}
			uid := *m.UserID
			if err := txStore.DeleteOrgPermission(customerID, uid); err != nil {
				return err
			}
			if err := reconcileNsPerms(txStore, uid, nil, nsByName); err != nil {
				return fmt.Errorf("%s: %w", m.Email, err)
			}
		}
		return nil
	})
}

// reconcileNsPerms makes the user's NamespacePermission rows match
// `want` exactly: upserts listed namespaces, deletes unlisted ones.
func reconcileNsPerms(s *Store, userID string, want map[string]NamespaceAcl, nsByName map[string]Namespace) error {
	current, err := s.ListUserNamespacePermissions(userID)
	if err != nil {
		return err
	}
	currentByNsID := make(map[uint]bool, len(current))
	for _, np := range current {
		currentByNsID[np.NamespaceID] = true
	}
	wantByNsID := make(map[uint]NamespaceAcl, len(want))
	for name, acl := range want {
		ns := nsByName[name]
		wantByNsID[ns.ID] = acl
	}
	for nsID, acl := range wantByNsID {
		err := s.UpsertNamespacePermission(&NamespacePermission{
			UserID:          userID,
			NamespaceID:     nsID,
			CanRead:         acl.Read,
			CanCreateTokens: acl.CreateTokens,
		})
		if err != nil {
			return err
		}
	}
	for nsID := range currentByNsID {
		if _, keep := wantByNsID[nsID]; !keep {
			if err := s.DeleteNamespacePermission(userID, nsID); err != nil {
				return err
			}
		}
	}
	return nil
}
