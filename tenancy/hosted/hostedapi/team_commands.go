package hostedapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
	"github.com/graybeardcorp/weft/tenancy/hosted"
	"github.com/graybeardcorp/weft/tenancy/hosted/auth"
)

// Team management is exposed in the shell as Unix-style commands —
// adduser, deluser, groups, id — that the shell ships through
// /api/shell/exec like any other command line. They're handled here in
// the api process rather than forwarded to the coordinator because they need
// things only it has: the email sender (invites), seat limits, and the owner
// session. The coordinator is a separate gRPC
// service for log queries and has none of that.
//
// Groups are the permission model: configmgr (the
// /etc manager) and, per namespace, "<ns>-r" (read) and "<ns>-w" (manage
// its crons, tokens and sources). A namespace's crons fire as its cron user,
// "<ns>-cron", whose reach is the read groups it has been added to. adduser and
// deluser change who is on the team; usermod changes the groups they or a cron
// user are in. See store/team_groups.go and store/cron_user.go.

// teamCommands is the set of standalone commands intercepted before the
// pipeline reaches the coordinator.
var teamCommands = map[string]bool{
	"adduser": true,
	"deluser": true,
	"usermod": true,
	"groups":  true,
	"id":      true,
}

// maybeHandleTeamCommand runs a pipeline as a team command when it is a
// single bare invocation of one (no pipes, redirects, or compounds).
// Returns handled=false for everything else so handleShellExec forwards
// to the coordinator unchanged.
func (p *product) maybeHandleTeamCommand(ctx context.Context, session *auth.Session, pipeline string) (handled bool, out string, err error) {
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
		out, err = p.teamID(session, args)
	case "groups":
		out, err = p.teamGroupsCmd(session, args)
	case "adduser":
		out, err = p.teamAddUser(ctx, session, args)
	case "deluser":
		out, err = p.teamDelUser(ctx, session, args)
	case "usermod":
		out, err = p.teamUsermod(ctx, session, args)
	}
	return true, out, err
}

// teamUsage is each team command's usage line, shown by --help and by
// argument errors.
var teamUsage = map[string]string{
	"adduser": "usage: adduser <email>\n  invite <email> to the org\n",
	"deluser": "usage: deluser <email>\n  remove <email> from the org\n",
	"usermod": "usage: usermod -aG <group> <user>\n       usermod -rG <group> <user>\n  add <user> to <group>, or remove it. <user> is a member's email or a\n  namespace's cron user (<namespace>-cron); a namespace has the groups\n  <namespace>-r (read) and <namespace>-w (manage its crons, tokens and sources).\n  A cron user can only join read groups: usermod -aG logs-r app-cron lets the\n  crons in app read logs. You need logs-r and app-w to do that.\n",
	"groups":  "usage: groups [<user>]\n  show a member's or cron user's groups (yours when no user is given)\n",
	"id":      "usage: id [<user>]\n  show a member's role and groups (yours when no user is given)\n",
}

// teamArgLimits is the (min, max) argument count per command.
var teamArgLimits = map[string][2]int{
	"adduser": {1, 1},
	"deluser": {1, 1},
	"usermod": {3, 3},
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
	for i, a := range args {
		if cmd == "usermod" && i == 0 && (a == "-aG" || a == "-rG") {
			continue
		}
		if strings.HasPrefix(a, "-") {
			return true, "", fmt.Errorf("%s: unknown option %s\n%s", cmd, a, strings.TrimRight(usage, "\n"))
		}
	}
	limits := teamArgLimits[cmd]
	if len(args) < limits[0] || len(args) > limits[1] {
		return true, "", errors.New(strings.TrimRight(usage, "\n"))
	}
	if cmd == "usermod" && args[0] != "-aG" && args[0] != "-rG" {
		return true, "", errors.New(strings.TrimRight(usage, "\n"))
	}
	// The user is an email address, or for the commands that show or change
	// groups, a namespace's cron user.
	who := ""
	switch cmd {
	case "usermod":
		who = args[2]
	default:
		if len(args) > 0 {
			who = args[0]
		}
	}
	if who != "" {
		isCron := cmd != "adduser" && cmd != "deluser" && strings.HasSuffix(who, "-cron") && !strings.Contains(who, "@")
		if addr, perr := mail.ParseAddress(who); !isCron && (perr != nil || addr.Address != who) {
			return true, "", fmt.Errorf("%s: %q is not an email address\n%s", cmd, who, strings.TrimRight(usage, "\n"))
		}
	}
	return false, "", nil
}

// teamID implements `id [<email>]` — the acting user when no email is
// given. Readable by any member.
func (p *product) teamID(session *auth.Session, args []string) (string, error) {
	email := session.Email
	if len(args) > 0 {
		email = args[0]
	}
	role, groups, err := p.groupsOf(session, email)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("uid=%s role=%s groups=%s\n", email, role, groupsLabel(role, groups, ",")), nil
}

// teamGroupsCmd implements `groups [<email>]`.
func (p *product) teamGroupsCmd(session *auth.Session, args []string) (string, error) {
	email := session.Email
	if len(args) > 0 {
		email = args[0]
	}
	role, groups, err := p.groupsOf(session, email)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s: %s\n", email, groupsLabel(role, groups, " ")), nil
}

// groupsOf is a member's role and groups, or those of a namespace's cron user.
func (p *product) groupsOf(session *auth.Session, user string) (role string, groups []string, err error) {
	if p.Store().IsCronUser(session.Org(), user) {
		groups, err = p.Store().CronUserGroups(session.Org(), user)
		return "cron", groups, err
	}
	role, _, groups, err = p.Store().MemberGroups(session.Org(), user)
	return role, groups, err
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

// teamAddUser implements `adduser <email>`: invite the email to the org,
// sending an invite email. Owner-only. Groups are usermod's.
func (p *product) teamAddUser(ctx context.Context, session *auth.Session, args []string) (string, error) {
	if p.Authorize(ctx, session.Caller(), hosted.ActionManageTeam, tenancy.Resource{}) != nil {
		return "", fmt.Errorf("only the owner can manage the team")
	}
	if len(args) != 1 {
		return "", fmt.Errorf("usage: adduser <email>")
	}
	email := args[0]
	if _, err := p.inviteMember(ctx, session, email); err != nil {
		return "", err
	}
	return fmt.Sprintf("Invited %s — an invite email is on its way. They'll join %s when they accept; then give them groups with `usermod -aG <group> %s`.\n",
		email, session.ActiveCustomerName, email), nil
}

// teamDelUser implements `deluser <email>`: remove the member from the org.
// Owner-only.
func (p *product) teamDelUser(ctx context.Context, session *auth.Session, args []string) (string, error) {
	if p.Authorize(ctx, session.Caller(), hosted.ActionManageTeam, tenancy.Resource{}) != nil {
		return "", fmt.Errorf("only the owner can manage the team")
	}
	if len(args) != 1 {
		return "", fmt.Errorf("usage: deluser <email>")
	}
	email := args[0]
	if email == session.Email {
		return "", fmt.Errorf("you can't remove yourself")
	}
	if err := p.Store().RemoveMember(session.Org(), email); err != nil {
		return "", err
	}
	return fmt.Sprintf("Removed %s from %s.\n", email, session.ActiveCustomerName), nil
}

// teamUsermod implements `usermod -aG|-rG <group> <user>`. For a member it is
// owner-only. For a namespace's cron user, adding it to a read group is the
// grant that lets that namespace's crons read the other namespace, and you may
// do it only if you hold what you hand out: write on the cron user's namespace
// and read on the group's. Removing needs write on the cron user's namespace.
func (p *product) teamUsermod(ctx context.Context, session *auth.Session, args []string) (string, error) {
	if len(args) != 3 {
		return "", errors.New(strings.TrimRight(teamUsage["usermod"], "\n"))
	}
	add, group, user := args[0] == "-aG", args[1], args[2]
	org := session.Org()

	if !strings.Contains(user, "@") {
		home, ok := p.Store().CronUserHome(org, user)
		if !ok {
			return "", fmt.Errorf("no such user: %s", user)
		}
		target, _, valid := store.SplitNamespaceGroup(group)
		if !valid {
			return "", fmt.Errorf("%s can only be added to a read group (<namespace>-r)", user)
		}
		act, res := tenancy.ActionManageCron, tenancy.Resource{Namespace: home}
		if add {
			act, res = tenancy.ActionGrantNamespaceRead, tenancy.Resource{Namespace: home, Target: target}
		}
		if err := p.Authorize(ctx, session.Caller(), act, res); err != nil {
			if add {
				return "", fmt.Errorf("to add %s to %s you need %s-w and %s-r", user, group, home, target)
			}
			return "", fmt.Errorf("to change %s you need %s-w", user, home)
		}
		if err := p.Store().SetCronUserGroup(org, user, group, add); err != nil {
			return "", err
		}
		if add {
			return fmt.Sprintf("Added %s to %s.\n", user, group), nil
		}
		return fmt.Sprintf("Removed %s from %s.\n", user, group), nil
	}

	if p.Authorize(ctx, session.Caller(), hosted.ActionManageTeam, tenancy.Resource{}) != nil {
		return "", fmt.Errorf("only the owner can manage the team")
	}
	if err := p.Store().ValidateGroup(org, group); err != nil {
		return "", err
	}
	if add {
		if err := p.Store().AddUserToGroup(org, user, group); err != nil {
			return "", err
		}
		return fmt.Sprintf("Added %s to %s.\n", user, group), nil
	}
	if err := p.Store().RemoveUserFromGroup(org, user, group); err != nil {
		return "", err
	}
	return fmt.Sprintf("Removed %s from %s.\n", user, group), nil
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
func (p *product) inviteMember(ctx context.Context, session *auth.Session, email string) (*store.TeamMember, error) {
	if p.Authorize(ctx, session.Caller(), hosted.ActionManageTeam, tenancy.Resource{}) != nil {
		return nil, &inviteErr{http.StatusForbidden, "only the owner can manage the team"}
	}
	tier := store.TierTrial
	if sub, _ := p.Store().ActiveSubscription(session.Org()); sub != nil {
		tier = store.Tier(sub.Tier)
	}
	count, _ := p.Store().CountTeamMembers(session.Org())
	if limit := store.SeatLimit(tier); count >= limit {
		return nil, &inviteErr{http.StatusForbidden, fmt.Sprintf("seat limit reached (%d/%d)", count, limit)}
	}
	if existing, _ := p.Store().GetTeamMemberByEmail(session.Org(), email); existing != nil {
		return nil, &inviteErr{http.StatusConflict, "already a team member"}
	}

	member, err := p.Store().InviteTeamMember(session.Org(), email, orgs.RoleMember, session.UserID)
	if err != nil {
		return nil, &inviteErr{http.StatusInternalServerError, "internal error"}
	}

	token, err := p.Store().IssueMagicLink(&store.MagicLink{
		Email:    email,
		Purpose:  store.MagicLinkInvite,
		InviteID: member.ID,
	})
	if err != nil {
		return nil, &inviteErr{http.StatusInternalServerError, "internal error"}
	}
	verifyURL := fmt.Sprintf("%s/verify?token=%s&invite=1", p.BaseURL(), token)
	orgName := session.ActiveCustomerName
	if cust, err := p.Store().GetOrgByName(session.ActiveCustomerName); err == nil && cust.DisplayName != "" {
		orgName = cust.DisplayName
	}
	SendInviteEmail(ctx, p.email, email, session.Email, orgName, verifyURL)
	return member, nil
}
