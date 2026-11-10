package mounts

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/graybeardcorp/weft/logreads"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy"
)

// A cron runs as the system identity, which reads every namespace, so what it
// may read can't be enforced as it runs. It is enforced when the cron is saved
// (authorizeCronReads), and again when a /bin script it runs is saved
// (authorizeBinEdit), since the script is part of the cron. Owners and the
// system identity read everything, so neither check applies to them.

// readsEnv describes the org to logreads. A script named in override is taken
// from there instead of the store, to ask what the org would read once a
// script is saved.
func (c Config) readsEnv(override map[string]string) (logreads.Env, error) {
	nss, err := c.Store.NamespaceNames(c.OrgID, "", true)
	if err != nil {
		return logreads.Env{}, err
	}
	return logreads.Env{
		// A cron, and the scripts it runs, name scripts by path.
		RequireBinPath: true,
		Namespaces:     nss,
		Bin: func(name string) (string, bool, bool) {
			if src, ok := override[name]; ok {
				return src, false, true
			}
			fn, err := c.Store.GetShellFunction(c.OrgID, name)
			if err != nil || fn == nil {
				return "", false, false
			}
			return fn.Source, fn.Language == "lua", true
		},
	}, nil
}

// denyReads is the refusal for a script that reads what the caller can't. It
// does not say which namespaces: a glob or a listing would name ones the
// caller may not see.
func denyReads(dir string) error {
	return fmt.Errorf("%s: %w (it reads log namespaces you aren't allowed to read)", dir, fs.ErrPermission)
}

// authorizeCronReads refuses a cron whose reads the saver can't be vouched for.
func (c Config) authorizeCronReads(pipeline string) error {
	_, all, err := c.scope()
	if err != nil || all { // a caller who reads everything has nothing to check
		return err
	}
	env, err := c.readsEnv(nil)
	if err != nil {
		return err
	}
	reads, err := logreads.Analyze(pipeline, env)
	var unresolvable *logreads.UnresolvableError
	if errors.As(err, &unresolvable) {
		return fmt.Errorf("this cron can't be saved: %w", unresolvable)
	}
	if err != nil {
		return nil // doesn't parse: the save reports that
	}
	return c.mustRead(reads.Namespaces, "/cron.d")
}

// authorizeHomedCronReads refuses a cron homed in a namespace that reads what
// its cron user can't. Its authority is the cron user's groups (its own
// namespace, plus any read group it was added to), not whoever saves it, so
// even an owner is held to it.
func (c Config) authorizeHomedCronReads(home, pipeline string) error {
	scoper, ok := c.Scope.(tenancy.NamespaceScoper)
	if !ok {
		return errors.New("mounts: the scope can't say what a namespace's crons may read")
	}
	set, err := scoper.NamespaceScope(c.ctx(), int64(c.OrgID), home)
	if err != nil {
		return err
	}
	allowed, all, err := set.Namespaces(int64(c.OrgID))
	if err != nil {
		return err
	}
	env, err := c.readsEnv(nil)
	if err != nil {
		return err
	}
	reads, err := logreads.Analyze(pipeline, env)
	var unresolvable *logreads.UnresolvableError
	if errors.As(err, &unresolvable) {
		return fmt.Errorf("this cron can't be saved: %w (a cron in a namespace must name what it reads)", unresolvable)
	}
	if err != nil {
		return nil // doesn't parse: the save reports that
	}
	for _, name := range reads.Namespaces {
		if !all && !slices.Contains(allowed, name) {
			return fmt.Errorf("/cron.d/%s: %w (it reads namespaces %s-cron can't read; `usermod -aG <namespace>-r %s-cron` adds one)", home, fs.ErrPermission, home, home)
		}
	}
	return nil
}

func (c Config) mustRead(namespaces []string, dir string) error {
	readable, _, err := c.scope()
	if err != nil {
		return err
	}
	for _, ns := range namespaces {
		if !slices.Contains(readable, ns) {
			return denyReads(dir)
		}
	}
	return nil
}

// authorizeBinEdit refuses a script that would make a cron already in the org
// read more than the editor may read, or that would leave a cron unresolvable.
// A script no cron runs is unrestricted: whoever runs it is held to their own
// read access as it executes.
func (c Config) authorizeBinEdit(name, source, lang string) error {
	if lang == "lua" { // a Lua script reads only its input
		return nil
	}
	_, all, err := c.scope()
	if err != nil || all {
		return err
	}
	crons, err := c.Store.ListCronWatches(c.OrgID)
	if err != nil || len(crons) == 0 {
		return err
	}
	before, err := c.readsEnv(nil)
	if err != nil {
		return err
	}
	after, err := c.readsEnv(map[string]string{name: source})
	if err != nil {
		return err
	}
	for _, cron := range crons {
		was, wasErr := logreads.Analyze(cron.Pipeline, before)
		now, nowErr := logreads.Analyze(cron.Pipeline, after)
		var unresolvable *logreads.UnresolvableError
		if errors.As(nowErr, &unresolvable) {
			// Only a cron that was resolvable and ran this script is made
			// worse by the change; one that was already unresolvable is not.
			if wasErr == nil && slices.Contains(was.Bins, name) {
				return fmt.Errorf("saving %s would leave the cron %q unresolvable: %w", name, cron.Name, unresolvable)
			}
			continue
		}
		if nowErr != nil || !slices.Contains(now.Bins, name) {
			continue
		}
		var added []string
		for _, ns := range now.Namespaces {
			if wasErr != nil || !slices.Contains(was.Namespaces, ns) {
				added = append(added, ns)
			}
		}
		if err := c.mustRead(added, "/bin"); err != nil {
			return err
		}
	}
	return nil
}

// mayWriteBin refuses a write to /bin/<name> unless the actor holds write on
// every namespace that has a cron running it (an owner holds it everywhere), or,
// for a script no cron runs, is a team member.
func (c Config) mayWriteBin(name string) error {
	crons, err := c.Store.CronsUsingBin(c.OrgID, name)
	if err != nil {
		return err
	}
	if len(crons) == 0 {
		if c.allowed(tenancy.ActionManageBin) {
			return nil
		}
		return fmt.Errorf("/bin/%s: %w (only team members change /bin)", name, fs.ErrPermission)
	}
	var need []string
	for _, cron := range crons {
		if cron.NamespaceID == nil {
			// An org-level cron has no namespace to hold write on: an owner's call.
			if !c.allowed(tenancy.ActionPublishOrgWide) {
				need = append(need, "an owner (an org-level cron runs it)")
			}
			continue
		}
		ns, err := c.Store.NamespaceName(c.OrgID, *cron.NamespaceID)
		if err != nil {
			return err
		}
		if !c.allowedIn(tenancy.ActionManageBin, ns) && !slices.Contains(need, ns+"-w") {
			need = append(need, ns+"-w")
		}
	}
	if len(need) > 0 {
		return fmt.Errorf("/bin/%s: %w (crons run this script, so changing it needs %s)", name, fs.ErrPermission, strings.Join(need, " and "))
	}
	return nil
}

// cronPaths renders crons as their /cron.d paths.
func (c Config) cronPaths(crons []store.CronWatch) ([]string, error) {
	var out []string
	for _, cron := range crons {
		if cron.NamespaceID == nil {
			out = append(out, "/cron.d/"+cron.Name)
			continue
		}
		ns, err := c.Store.NamespaceName(c.OrgID, *cron.NamespaceID)
		if err != nil {
			return nil, err
		}
		out = append(out, "/cron.d/"+ns+"/"+cron.Name)
	}
	return out, nil
}
