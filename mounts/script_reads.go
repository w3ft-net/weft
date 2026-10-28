package mounts

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/graybeardcorp/weft/logreads"
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
		Namespaces: nss,
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

// exempt reports whether the caller reads everything, so there is nothing to
// check.
func (c Config) exempt() bool { return c.UserID == "" || c.IsOwner }

// denyReads is the refusal for a script that reads what the caller can't. It
// does not say which namespaces: a glob or a listing would name ones the
// caller may not see.
func denyReads(dir string) error {
	return fmt.Errorf("%s: %w (it reads log namespaces you aren't allowed to read)", dir, fs.ErrPermission)
}

// authorizeCronReads refuses a cron whose reads the saver can't be vouched for.
func (c Config) authorizeCronReads(pipeline string) error {
	if c.exempt() {
		return nil
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

func (c Config) mustRead(namespaces []string, dir string) error {
	readable, err := c.Store.ReadableNamespaces(c.OrgID, c.UserID, false)
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
	if c.exempt() || lang == "lua" { // a Lua script reads only its input
		return nil
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
