package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kevlarcon/weft/auth"
	"github.com/kevlarcon/weft/store"
)

// Team management is exposed in the shell as Unix-style commands —
// adduser, deluser, groups, id — that the shell ships through
// /api/shell/exec like any other command line. They're handled here at
// the api layer rather than forwarded to the coordinator because they
// need things only the api Server has: the email sender (invites), seat
// limits, and the owner session. The coordinator is a separate gRPC
// service for log queries and has none of that.
//
// Groups are the permission model: binmgr/cronmgr/configmgr (the
// org-dir managers), a namespace name (read), or "<ns>+tokens" (mint
// producer tokens). See store/team_groups.go.

// teamCommands is the set of standalone commands intercepted before the
// pipeline reaches the coordinator.
var teamCommands = map[string]bool{
	"adduser": true,
	"deluser": true,
	"groups":  true,
	"id":      true,
}

// maybeHandleTeamCommand runs a pipeline as a team command when it is a
// single bare invocation of one (no pipes, redirects, or compounds).
// Returns handled=false for everything else so handleShellExec forwards
// to the coordinator unchanged.
func (s *Server) maybeHandleTeamCommand(session *auth.Session, pipeline string) (handled bool, out string, err error) {
	fields := strings.Fields(pipeline)
	if len(fields) == 0 || !teamCommands[fields[0]] {
		return false, "", nil
	}
	// Only a standalone command — anything with shell plumbing falls
	// through to the normal pipeline path (where it'll simply error,
	// since these aren't coordinator commands).
	if strings.ContainsAny(pipeline, "|&;<>`$()") {
		return false, "", nil
	}

	cmd, args := fields[0], fields[1:]
	switch cmd {
	case "id":
		out, err = s.teamID(session, args)
	case "groups":
		out, err = s.teamGroupsCmd(session, args)
	case "adduser":
		out, err = s.teamAddUser(session, args)
	case "deluser":
		out, err = s.teamDelUser(session, args)
	}
	return true, out, err
}

// teamID implements `id [<email>]` — the acting user when no email is
// given. Readable by any member.
func (s *Server) teamID(session *auth.Session, args []string) (string, error) {
	email := session.Email
	if len(args) > 0 {
		email = args[0]
	}
	role, _, groups, err := s.store.MemberGroups(session.ActiveCustomerID, email)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("uid=%s role=%s groups=%s\n", email, role, groupsLabel(role, groups, ",")), nil
}

// teamGroupsCmd implements `groups [<email>]`.
func (s *Server) teamGroupsCmd(session *auth.Session, args []string) (string, error) {
	email := session.Email
	if len(args) > 0 {
		email = args[0]
	}
	role, _, groups, err := s.store.MemberGroups(session.ActiveCustomerID, email)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s: %s\n", email, groupsLabel(role, groups, " ")), nil
}

// groupsLabel renders a member's group list, collapsing owners to the
// implicit-all marker and empty membership to a dash.
func groupsLabel(role string, groups []string, sep string) string {
	if role == "owner" {
		return "(all — implicit)"
	}
	if len(groups) == 0 {
		return "—"
	}
	return strings.Join(groups, sep)
}

// teamAddUser implements `adduser <email> [<group>]`. With no group it
// invites the email to the org (sending an invite email); with a group
// it grants the membership. Owner-only.
func (s *Server) teamAddUser(session *auth.Session, args []string) (string, error) {
	if !session.IsOwner {
		return "", fmt.Errorf("only the owner can manage the team")
	}
	if len(args) == 0 {
		return "", fmt.Errorf("usage: adduser <email> [<group>]")
	}
	email := args[0]
	if len(args) == 1 {
		if _, err := s.inviteMember(session, email); err != nil {
			return "", err
		}
		return fmt.Sprintf("Invited %s — an invite email is on its way. They'll join %s when they accept.\n",
			email, session.ActiveCustomerName), nil
	}
	group := args[1]
	if err := s.store.AddUserToGroup(session.ActiveCustomerID, email, group); err != nil {
		return "", err
	}
	return fmt.Sprintf("Added %s to %s.\n", email, group), nil
}

// teamDelUser implements `deluser <email> [<group>]`. With no group it
// removes the member from the org; with a group it revokes that
// membership. Owner-only.
func (s *Server) teamDelUser(session *auth.Session, args []string) (string, error) {
	if !session.IsOwner {
		return "", fmt.Errorf("only the owner can manage the team")
	}
	if len(args) == 0 {
		return "", fmt.Errorf("usage: deluser <email> [<group>]")
	}
	email := args[0]
	if len(args) == 1 {
		if email == session.Email {
			return "", fmt.Errorf("you can't remove yourself")
		}
		if err := s.store.RemoveMember(session.ActiveCustomerID, email); err != nil {
			return "", err
		}
		return fmt.Sprintf("Removed %s from %s.\n", email, session.ActiveCustomerName), nil
	}
	group := args[1]
	if err := s.store.RemoveUserFromGroup(session.ActiveCustomerID, email, group); err != nil {
		return "", err
	}
	return fmt.Sprintf("Removed %s from %s.\n", email, group), nil
}

// inviteErr carries an HTTP status so the JSON invite handler can keep
// precise codes while the shell-command path just surfaces the message.
type inviteErr struct {
	status int
	msg    string
}

func (e *inviteErr) Error() string { return e.msg }

// inviteMember creates a pending team membership for email and emails an
// invite link. Shared by the JSON endpoint (handleInviteTeamMember) and
// the `adduser <email>` command. Returns an *inviteErr for the known
// rejection cases (seat limit, duplicate).
func (s *Server) inviteMember(session *auth.Session, email string) (*store.TeamMember, error) {
	tier := store.TierTrial
	if sub, _ := s.store.ActiveSubscription(session.ActiveCustomerID); sub != nil {
		tier = store.Tier(sub.Tier)
	}
	count, _ := s.store.CountTeamMembers(session.ActiveCustomerID)
	if limit := store.SeatLimit(tier); count >= limit {
		return nil, &inviteErr{http.StatusForbidden, fmt.Sprintf("seat limit reached (%d/%d)", count, limit)}
	}
	if existing, _ := s.store.GetTeamMemberByEmail(session.ActiveCustomerID, email); existing != nil {
		return nil, &inviteErr{http.StatusConflict, "already a team member"}
	}

	member := &store.TeamMember{
		ID:         uuid.New().String(),
		CustomerID: session.ActiveCustomerID,
		Email:      email,
		Role:       "member",
		Status:     "pending",
		InvitedBy:  session.UserID,
		InvitedAt:  time.Now(),
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if err := s.store.CreateTeamMember(member); err != nil {
		return nil, &inviteErr{http.StatusInternalServerError, "internal error"}
	}

	token, err := s.store.CreateMagicLink(email)
	if err != nil {
		return nil, &inviteErr{http.StatusInternalServerError, "internal error"}
	}
	verifyURL := fmt.Sprintf("%s/verify?token=%s&invite=1", s.baseURL, token)
	orgName := session.ActiveCustomerName
	if cust, err := s.store.GetCustomerByName(session.ActiveCustomerName); err == nil && cust.DisplayName != "" {
		orgName = cust.DisplayName
	}
	SendInviteEmail(s.email, email, session.Email, orgName, verifyURL)
	return member, nil
}
