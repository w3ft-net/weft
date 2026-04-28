package proposals

import (
	"github.com/pmezard/go-difflib/difflib"
)

// renderDiff produces a unified diff between the current and
// proposed bodies of a proposal. Path labels follow the kind so a
// human reviewer sees realistic file paths in the diff header:
//
//	bin/<name>          for script proposals
//	cron.d/<name>       for cron proposals
//
// Delete proposals render the proposed-side label as /dev/null,
// matching the conventional patch shape for a removal.
func renderDiff(p Proposal) string {
	fromLabel, toLabel := pathLabels(p.Kind, p.SubKind, p.Name)

	d := difflib.UnifiedDiff{
		A:        difflib.SplitLines(p.Current),
		B:        difflib.SplitLines(p.Proposed),
		FromFile: fromLabel,
		ToFile:   toLabel,
		Context:  3,
	}
	out, err := difflib.GetUnifiedDiffString(d)
	if err != nil {
		// difflib.GetUnifiedDiffString does not actually return errors
		// for the inputs we use; this branch exists only to satisfy the
		// signature.
		return ""
	}
	return out
}

// pathLabels returns the (from, to) labels used in the diff header.
// For deletes, the resource is going away, so to = /dev/null. For
// creations (Current == "" but the caller didn't say "delete"), the
// from label still names the resource so reviewers see what file is
// being added — convention matches `git diff` output for a new file.
func pathLabels(kind, subKind, name string) (string, string) {
	if kind == "delete" {
		return resourcePath(subKind, name), "/dev/null"
	}
	path := resourcePath(kind, name)
	return path, path
}

func resourcePath(kind, name string) string {
	switch kind {
	case "script":
		return "bin/" + name
	case "cron":
		return "cron.d/" + name
	}
	return name
}
