package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/store"
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
func (s *Server) maybeHandleTeamCommand(ctx context.Context, session *auth.Session, pipeline string) (handled bool, out string, err error) {
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
	if done, out, err := checkTeamArgs(cmd, args); done {
		return true, out, err
	}
	switch cmd {
	case "id":
		out, err = s.teamID(session, args)
	case "groups":
		out, err = s.teamGroupsCmd(session, args)
	case "adduser":
		out, err = s.teamAddUser(ctx, session, args)
	case "deluser":
		out, err = s.teamDelUser(session, args)
	}
	return true, out, err
}

// teamUsage is each team command's usage line, shown by --help and by
// argument errors.
var teamUsage = map[string]string{
	"adduser": "usage: adduser <email> [<group>]\n  invite <email> to the org, or add an existing member to <group>\n",
	"deluser": "usage: deluser <email> [<group>]\n  remove <email> from the org, or from <group>\n",
	"groups":  "usage: groups [<email>]\n  show a member's groups (yours when no email is given)\n",
	"id":      "usage: id [<email>]\n  show a member's role and groups (yours when no email is given)\n",
}

// teamArgLimits is the (min, max) argument count per command.
var teamArgLimits = map[string][2]int{
	"adduser": {1, 2},
	"deluser": {1, 2},
	"groups":  {0, 1},
	"id":      {0, 1},
}

// checkTeamArgs validates a team command's arguments before anything runs,
// so a mistyped flag or word is never treated as an email address (an
// `adduser --help` once invited a user named "--help"). done reports that
// the command is finished: out holds the --help text, err a usage error.
// Help and argument errors are not privileged, so this runs ahead of the
// owner check.
func checkTeamArgs(cmd string, args []string) (done bool, out string, err error) {
	usage := teamUsage[cmd]
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true, usage, nil
		}
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return true, "", fmt.Errorf("%s: unknown option %s\n%s", cmd, a, strings.TrimRight(usage, "\n"))
		}
	}
	limits := teamArgLimits[cmd]
	if len(args) < limits[0] || len(args) > limits[1] {
		return true, "", errors.New(strings.TrimRight(usage, "\n"))
	}
	if len(args) > 0 {
		// The first argument is always an email address.
		if addr, perr := mail.ParseAddress(args[0]); perr != nil || addr.Address != args[0] {
			return true, "", fmt.Errorf("%s: %q is not an email address\n%s", cmd, args[0], strings.TrimRight(usage, "\n"))
		}
	}
	return false, "", nil
}

// teamID implements `id [<email>]` — the acting user when no email is
// given. Readable by any member.
func (s *Server) teamID(session *auth.Session, args []string) (string, error) {
	email := session.Email
	if len(args) > 0 {
		email = args[0]
	}
	role, _, groups, err := s.store.MemberGroups(session.ActiveOrgID, email)
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
	role, _, groups, err := s.store.MemberGroups(session.ActiveOrgID, email)
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
func (s *Server) teamAddUser(ctx context.Context, session *auth.Session, args []string) (string, error) {
	if !session.IsOwner {
		return "", fmt.Errorf("only the owner can manage the team")
	}
	if len(args) == 0 {
		return "", fmt.Errorf("usage: adduser <email> [<group>]")
	}
	email := args[0]
	custID := session.ActiveOrgID
	if len(args) == 1 {
		if _, err := s.inviteMember(ctx, session, email); err != nil {
			return "", err
		}
		return fmt.Sprintf("Invited %s — an invite email is on its way. They'll join %s when they accept.\n",
			email, session.ActiveCustomerName), nil
	}

	group := args[1]
	// Reject a bad group up front, before any side effect — otherwise a
	// typo'd group would invite someone we didn't mean to.
	if err := s.store.ValidateGroup(custID, group); err != nil {
		return "", err
	}

	// We don't stage grants against unaccepted invites. Instead, make the
	// state explicit: a brand-new email gets invited now (the email goes
	// out) and the owner re-runs once they've joined; an already-invited
	// member who hasn't accepted gets a clear error rather than silence.
	member, _ := s.store.GetTeamMemberByEmail(custID, email)
	switch {
	case member == nil:
		if _, err := s.inviteMember(ctx, session, email); err != nil {
			return "", err
		}
		return fmt.Sprintf("Invited %s — an invite email is on its way. They must accept before joining %s; re-run `adduser %s %s` once they have.\n",
			email, group, email, group), nil
	case member.Pending():
		return "", fmt.Errorf("%s has been invited but hasn't accepted yet — re-run `adduser %s %s` after they accept", email, email, group)
	}

	if err := s.store.AddUserToGroup(custID, email, group); err != nil {
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
		if err := s.store.RemoveMember(session.ActiveOrgID, email); err != nil {
			return "", err
		}
		return fmt.Sprintf("Removed %s from %s.\n", email, session.ActiveCustomerName), nil
	}
	group := args[1]
	if err := s.store.RemoveUserFromGroup(session.ActiveOrgID, email, group); err != nil {
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
// rejection cases (not the owner, seat limit, duplicate).
func (s *Server) inviteMember(ctx context.Context, session *auth.Session, email string) (*store.TeamMember, error) {
	if !session.IsOwner {
		return nil, &inviteErr{http.StatusForbidden, "only the owner can manage the team"}
	}
	tier := store.TierTrial
	if sub, _ := s.store.ActiveSubscription(session.ActiveOrgID); sub != nil {
		tier = store.Tier(sub.Tier)
	}
	count, _ := s.store.CountTeamMembers(session.ActiveOrgID)
	if limit := store.SeatLimit(tier); count >= limit {
		return nil, &inviteErr{http.StatusForbidden, fmt.Sprintf("seat limit reached (%d/%d)", count, limit)}
	}
	if existing, _ := s.store.GetTeamMemberByEmail(session.ActiveOrgID, email); existing != nil {
		return nil, &inviteErr{http.StatusConflict, "already a team member"}
	}

	member, err := s.store.InviteTeamMember(session.ActiveOrgID, email, orgs.RoleMember, session.UserID)
	if err != nil {
		return nil, &inviteErr{http.StatusInternalServerError, "internal error"}
	}

	token, err := s.store.IssueMagicLink(&store.MagicLink{
		Email:    email,
		Purpose:  store.MagicLinkInvite,
		InviteID: member.ID,
	})
	if err != nil {
		return nil, &inviteErr{http.StatusInternalServerError, "internal error"}
	}
	verifyURL := fmt.Sprintf("%s/verify?token=%s&invite=1", s.baseURL, token)
	orgName := session.ActiveCustomerName
	if cust, err := s.store.GetOrgByName(session.ActiveCustomerName); err == nil && cust.DisplayName != "" {
		orgName = cust.DisplayName
	}
	SendInviteEmail(ctx, s.email, email, session.Email, orgName, verifyURL)
	return member, nil
}
