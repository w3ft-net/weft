package binscript

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/logreads"
	"github.com/w3ft-net/weft/shape"
)

// Service is what a build does with /bin scripts beyond storing them: it judges
// a script before it is saved and answers which crons run it. The crons that run
// a script are found by analysing each cron's pipeline, not recorded, so a
// script that changes what it calls changes its dependents with no bookkeeping.
type Service struct {
	Bins    bin.Store
	Crons   cron.Store
	Sources source.Store
}

// Save validates the script and writes it. language is bin.LangBash or
// bin.LangLua, and a Lua source has its shebang stripped already.
func (s Service) Save(owner shape.Added, user, name, source, language string) error {
	if err := Validate(language, source); err != nil {
		return err
	}
	refs, err := Refs(language, source)
	if err != nil {
		return err
	}
	if err := s.RefuseTextRefs(owner, refs, ""); err != nil {
		return err
	}
	return s.Bins.Save(owner, user, name, source, language)
}

// RefuseTextRefs refuses numeric reads, by literal <namespace>/<name>, of crons
// that store text. skip is the cron being saved, whose own type its saver
// checks. A reference that names no namespace is refused too: it can't resolve.
// A reference to a cron or namespace that doesn't exist is let through.
func (s Service) RefuseTextRefs(owner shape.Added, refs []string, skip string) error {
	for _, ref := range refs {
		if ref == skip {
			continue
		}
		nsName, name, ok := strings.Cut(ref, "/")
		if !ok {
			return fmt.Errorf("a cron is named by its namespace and name, <namespace>/<name>; %q has no namespace", ref)
		}
		ns, found, err := s.Sources.GetNamespace(owner, nsName)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		w, err := s.Crons.GetWatch(owner, ns.ID, name)
		if errors.Is(err, cron.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if w.Compiled.ValueType == cron.ValueText {
			return fmt.Errorf("cron %q stores text (# type: text); numeric weft functions can't read it, but weft::latest (weft.latest in Lua) can", ref)
		}
	}
	return nil
}

// Dependents lists the crons that run the script, however indirectly: those
// whose pipeline analyses as running it, and those whose pipeline names
// /bin/<name> even though the script doesn't exist yet, so that creating it can't
// slip past the crons waiting for it.
func (s Service) Dependents(owner shape.Added, name string) ([]cron.Watch, error) {
	watches, err := s.Crons.ListWatches(owner)
	if err != nil {
		return nil, err
	}
	env, err := s.env(owner)
	if err != nil {
		return nil, err
	}
	named := regexp.MustCompile(`/bin/` + regexp.QuoteMeta(name) + `($|[^A-Za-z0-9_.-])`)
	var out []cron.Watch
	for _, w := range watches {
		if named.MatchString(w.Pipeline) {
			out = append(out, w)
			continue
		}
		if reads, err := logreads.Analyze(w.Pipeline, env); err == nil && slices.Contains(reads.Bins, name) {
			out = append(out, w)
		}
	}
	return out, nil
}

// env describes the owner's scripts and namespaces to the analyzer.
func (s Service) env(owner shape.Added) (logreads.Env, error) {
	nss, err := s.Sources.ListNamespaces(owner)
	if err != nil {
		return logreads.Env{}, err
	}
	names := make([]string, len(nss))
	for i, n := range nss {
		names[i] = n.Name
	}
	return logreads.Env{
		Namespaces: names,
		Bin: func(name string) (string, bool, bool) {
			sc, err := s.Bins.Get(owner, name)
			if err != nil {
				return "", false, false
			}
			return sc.Source, sc.Language == bin.LangLua, true
		},
	}, nil
}

// InUseError is the refusal to delete a script crons still run.
type InUseError struct {
	Name  string
	Crons []cron.Watch
}

func (e *InUseError) Error() string {
	names := make([]string, len(e.Crons))
	for i, c := range e.Crons {
		names[i] = c.Name
	}
	return fmt.Sprintf("/bin/%s is run by crons: %s", e.Name, strings.Join(names, ", "))
}

// Delete removes the script, unless a cron runs it (*InUseError).
func (s Service) Delete(owner shape.Added, name string) error {
	crons, err := s.Dependents(owner, name)
	if err != nil {
		return err
	}
	if len(crons) > 0 {
		return &InUseError{Name: name, Crons: crons}
	}
	return s.Bins.Delete(owner, name)
}
