package coordinator

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/kevlarcon/warp/pkg/bash"
	"github.com/kevlarcon/weft/store"
)

// WitFuncs holds the request-scoped state the `wit` builtin needs:
// the store handle, the active customer's ID, and the user driving
// the request. One per request — wired by the coordinator the same
// way WeftFuncs is.
type WitFuncs struct {
	Store      *store.Store
	CustomerID uint
	UserID     string // user invoking; empty = system call (still works for read-only paths)
}

// Run is the dispatcher. Subcommands look up by name and route to
// per-subcommand methods. Returns the exit code; 0 success, 1
// usage / not-found / store error, 2 reserved for unrecognised
// flags (unused today).
func (w *WitFuncs) Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: wit <log|show|diff|checkout|amend> [args...]")
		return 1
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "log":
		return w.runLog(rest, stdout, stderr)
	case "show":
		return w.runShow(rest, stdout, stderr)
	case "diff":
		return w.runDiff(rest, stdout, stderr)
	case "checkout":
		return w.runCheckout(rest, stdout, stderr)
	case "amend":
		return w.runAmend(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "wit: unknown subcommand %q\n", sub)
		return 1
	}
}

// RegisterWitCommands registers `wit` as a bash command. The
// dispatcher (Run) does all the per-subcommand work.
func RegisterWitCommands(interp *bash.Interpreter, wf *WitFuncs) {
	interp.RegisterCommand("wit", func(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
		return wf.Run(args, stdin, stdout, stderr)
	})
}

// pathKind classifies a /bin/foo or /cron.d/foo path. Returns
// ("bin", "foo") or ("cron.d", "foo"); empty kind for any other
// namespace, which the caller renders as "history not tracked here".
func pathKind(path string) (kind, name string) {
	p := strings.TrimPrefix(path, "/")
	if i := strings.Index(p, "/"); i > 0 {
		return p[:i], p[i+1:]
	}
	return "", ""
}

// requirePath validates that argv has a path arg in the expected
// position and that the path lives in /bin or /cron.d. Returns the
// (kind, name, ok) tuple; on error writes a usage message and
// returns ok=false.
func requirePath(stderr io.Writer, sub, path string) (kind, name string, ok bool) {
	if path == "" {
		fmt.Fprintf(stderr, "usage: wit %s <path> [...]\n", sub)
		return "", "", false
	}
	k, n := pathKind(path)
	if k != "bin" && k != "cron.d" {
		fmt.Fprintf(stderr, "wit %s: history not tracked here (%s)\n", sub, path)
		return "", "", false
	}
	return k, n, true
}

// log -------------------------------------------------------------

func (w *WitFuncs) runLog(args []string, stdout, stderr io.Writer) int {
	patch := false
	var path string
	for _, a := range args {
		switch a {
		case "-p":
			patch = true
		default:
			if strings.HasPrefix(a, "-") {
				fmt.Fprintf(stderr, "wit log: unknown flag %s\n", a)
				return 1
			}
			path = a
		}
	}
	kind, name, ok := requirePath(stderr, "log", path)
	if !ok {
		return 1
	}
	switch kind {
	case "bin":
		return w.logBin(name, patch, stdout, stderr)
	case "cron.d":
		return w.logCron(name, patch, stdout, stderr)
	}
	return 1 // unreachable
}

func (w *WitFuncs) logBin(name string, patch bool, stdout, stderr io.Writer) int {
	versions, err := w.Store.LogShellFunction(w.CustomerID, name)
	if err != nil {
		fmt.Fprintf(stderr, "wit log: %v\n", err)
		return 1
	}
	if len(versions) == 0 {
		fmt.Fprintf(stderr, "wit log: /bin/%s not found\n", name)
		return 1
	}
	for i, v := range versions {
		writeLogLine(stdout, i, v.ContentHash, v.Message, v.AuthoredAt.Format("2006-01-02 15:04"),
			ownerLabel(w.Store, v.AuthoredByUserID), v.IsHead)
		if patch && i+1 < len(versions) {
			writeDiffSection(stdout,
				versions[i+1].Source, versions[i].Source,
				refLabel(i+1)+" "+versions[i+1].ContentHash[:7],
				refLabel(i)+" "+versions[i].ContentHash[:7])
		}
	}
	return 0
}

func (w *WitFuncs) logCron(name string, patch bool, stdout, stderr io.Writer) int {
	versions, err := w.Store.LogCronWatch(w.CustomerID, name)
	if err != nil {
		fmt.Fprintf(stderr, "wit log: %v\n", err)
		return 1
	}
	if len(versions) == 0 {
		fmt.Fprintf(stderr, "wit log: /cron.d/%s not found\n", name)
		return 1
	}
	for i, v := range versions {
		writeLogLine(stdout, i, v.ContentHash, v.Message, v.AuthoredAt.Format("2006-01-02 15:04"),
			ownerLabel(w.Store, v.AuthoredByUserID), v.IsHead)
		if patch && i+1 < len(versions) {
			writeDiffSection(stdout,
				versions[i+1].Schedule+"\n"+versions[i+1].Pipeline,
				versions[i].Schedule+"\n"+versions[i].Pipeline,
				refLabel(i+1)+" "+versions[i+1].ContentHash[:7],
				refLabel(i)+" "+versions[i].ContentHash[:7])
		}
	}
	return 0
}

// writeLogLine renders one row of `wit log` output.
//
//	a4fb0c2  HEAD     2026-04-25 09:42  Alice   "msg"
func writeLogLine(w io.Writer, idx int, hash, message, when, owner string, isHead bool) {
	short := "       "
	if len(hash) >= 7 {
		short = hash[:7]
	}
	ref := refLabel(idx)
	headMark := " "
	if isHead {
		headMark = "*"
	}
	msg := ""
	if message != "" {
		msg = `  "` + firstLine(message) + `"`
	}
	fmt.Fprintf(w, "%s %s  %-7s  %s  %s%s\n", headMark, short, ref, when, owner, msg)
}

func refLabel(idx int) string {
	switch idx {
	case 0:
		return "HEAD"
	case 1:
		return "HEAD^"
	default:
		return fmt.Sprintf("HEAD^%d", idx)
	}
}

// firstLine returns everything before the first newline.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ownerLabel renders a user reference into the same display label
// `ls -l` uses, by reusing shellfs's resolveOwners semantics in a
// one-off lookup. Returns "-" when the user can't be resolved.
func ownerLabel(s *store.Store, userID *string) string {
	if userID == nil || *userID == "" {
		return "-"
	}
	users, err := s.GetUsersByIDs([]string{*userID})
	if err != nil {
		return (*userID)[:8]
	}
	if u, ok := users[*userID]; ok {
		if u.Name != "" {
			return truncate(u.Name, 12)
		}
		if at := strings.IndexByte(u.Email, '@'); at > 0 {
			return truncate(u.Email[:at], 12)
		}
	}
	return truncate(*userID, 12)
}

// writeDiffSection emits a unified-diff block between two version
// contents, labeled with the supplied refs. Used by log -p between
// consecutive entries.
func writeDiffSection(w io.Writer, oldContent, newContent, oldLabel, newLabel string) {
	fmt.Fprintf(w, "diff %s ← %s:\n", newLabel, oldLabel)
	dmp := diffmatchpatch.New()
	a, b, lines := dmp.DiffLinesToRunes(oldContent, newContent)
	diffs := dmp.DiffMainRunes(a, b, false)
	diffs = dmp.DiffCharsToLines(diffs, lines)
	for _, d := range diffs {
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			// Skip context for compactness.
		case diffmatchpatch.DiffInsert:
			for _, line := range strings.SplitAfter(d.Text, "\n") {
				if line == "" {
					continue
				}
				if !strings.HasSuffix(line, "\n") {
					line += "\n"
				}
				fmt.Fprintf(w, "+ %s", line)
			}
		case diffmatchpatch.DiffDelete:
			for _, line := range strings.SplitAfter(d.Text, "\n") {
				if line == "" {
					continue
				}
				if !strings.HasSuffix(line, "\n") {
					line += "\n"
				}
				fmt.Fprintf(w, "- %s", line)
			}
		}
	}
}

// show ------------------------------------------------------------

func (w *WitFuncs) runShow(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: wit show <path> <ref>")
		return 1
	}
	kind, name, ok := requirePath(stderr, "show", args[0])
	if !ok {
		return 1
	}
	ref := args[1]
	switch kind {
	case "bin":
		content, err := w.Store.ShellFunctionContent(w.CustomerID, name, ref)
		if err != nil {
			fmt.Fprintf(stderr, "wit show: %v\n", err)
			return 1
		}
		fmt.Fprint(stdout, content)
		if !strings.HasSuffix(content, "\n") {
			fmt.Fprintln(stdout)
		}
	case "cron.d":
		schedule, pipeline, err := w.Store.CronWatchContent(w.CustomerID, name, ref)
		if err != nil {
			fmt.Fprintf(stderr, "wit show: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "# schedule: %s\n%s", schedule, pipeline)
		if !strings.HasSuffix(pipeline, "\n") {
			fmt.Fprintln(stdout)
		}
	}
	return 0
}

// diff ------------------------------------------------------------

func (w *WitFuncs) runDiff(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: wit diff <path> <a>..<b>")
		return 1
	}
	kind, name, ok := requirePath(stderr, "diff", args[0])
	if !ok {
		return 1
	}
	rangeArg := args[1]
	parts := strings.SplitN(rangeArg, "..", 2)
	if len(parts) != 2 {
		fmt.Fprintf(stderr, "wit diff: range must be <a>..<b>; got %q\n", rangeArg)
		return 1
	}
	aRef, bRef := parts[0], parts[1]

	switch kind {
	case "bin":
		va, err := w.Store.ResolveShellFunctionRef(w.CustomerID, name, aRef)
		if err != nil {
			fmt.Fprintf(stderr, "wit diff: %v\n", err)
			return 1
		}
		vb, err := w.Store.ResolveShellFunctionRef(w.CustomerID, name, bRef)
		if err != nil {
			fmt.Fprintf(stderr, "wit diff: %v\n", err)
			return 1
		}
		writeUnifiedDiff(stdout, name, va.Source, vb.Source,
			va.ContentHash, vb.ContentHash,
			ownerLabel(w.Store, va.AuthoredByUserID),
			ownerLabel(w.Store, vb.AuthoredByUserID))
	case "cron.d":
		va, err := w.Store.ResolveCronWatchRef(w.CustomerID, name, aRef)
		if err != nil {
			fmt.Fprintf(stderr, "wit diff: %v\n", err)
			return 1
		}
		vb, err := w.Store.ResolveCronWatchRef(w.CustomerID, name, bRef)
		if err != nil {
			fmt.Fprintf(stderr, "wit diff: %v\n", err)
			return 1
		}
		writeUnifiedDiff(stdout, name,
			va.Schedule+"\n"+va.Pipeline,
			vb.Schedule+"\n"+vb.Pipeline,
			va.ContentHash, vb.ContentHash,
			ownerLabel(w.Store, va.AuthoredByUserID),
			ownerLabel(w.Store, vb.AuthoredByUserID))
	}
	return 0
}

// writeUnifiedDiff emits a diff with --- a/<name> / +++ b/<name>
// headers and short-hash + owner annotations.
func writeUnifiedDiff(w io.Writer, name, oldContent, newContent, oldHash, newHash, oldOwner, newOwner string) {
	short := func(h string) string {
		if len(h) >= 7 {
			return h[:7]
		}
		return h
	}
	fmt.Fprintf(w, "--- a/%s (%s, %s)\n", name, short(oldHash), oldOwner)
	fmt.Fprintf(w, "+++ b/%s (%s, %s)\n", name, short(newHash), newOwner)
	dmp := diffmatchpatch.New()
	a, b, lines := dmp.DiffLinesToRunes(oldContent, newContent)
	diffs := dmp.DiffMainRunes(a, b, false)
	diffs = dmp.DiffCharsToLines(diffs, lines)
	for _, d := range diffs {
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			// Skip context lines in this minimal renderer.
		case diffmatchpatch.DiffInsert:
			for _, line := range strings.SplitAfter(d.Text, "\n") {
				if line == "" {
					continue
				}
				if !strings.HasSuffix(line, "\n") {
					line += "\n"
				}
				fmt.Fprintf(w, "+ %s", line)
			}
		case diffmatchpatch.DiffDelete:
			for _, line := range strings.SplitAfter(d.Text, "\n") {
				if line == "" {
					continue
				}
				if !strings.HasSuffix(line, "\n") {
					line += "\n"
				}
				fmt.Fprintf(w, "- %s", line)
			}
		}
	}
}

// checkout --------------------------------------------------------

func (w *WitFuncs) runCheckout(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: wit checkout <path> <ref>")
		return 1
	}
	kind, name, ok := requirePath(stderr, "checkout", args[0])
	if !ok {
		return 1
	}
	ref := args[1]
	var hash string
	switch kind {
	case "bin":
		v, err := w.Store.ResolveShellFunctionRef(w.CustomerID, name, ref)
		if err != nil {
			fmt.Fprintf(stderr, "wit checkout: %v\n", err)
			return 1
		}
		if err := w.Store.CheckoutShellFunction(w.CustomerID, w.UserID, name, ref); err != nil {
			fmt.Fprintf(stderr, "wit checkout: %v\n", err)
			return 1
		}
		hash = v.ContentHash
	case "cron.d":
		v, err := w.Store.ResolveCronWatchRef(w.CustomerID, name, ref)
		if err != nil {
			fmt.Fprintf(stderr, "wit checkout: %v\n", err)
			return 1
		}
		if err := w.Store.CheckoutCronWatch(w.CustomerID, w.UserID, name, ref); err != nil {
			fmt.Fprintf(stderr, "wit checkout: %v\n", err)
			return 1
		}
		hash = v.ContentHash
	}
	short := hash
	if len(hash) >= 7 {
		short = hash[:7]
	}
	fmt.Fprintf(stdout, "checked out /%s/%s @ %s  (now HEAD)\n", kind, name, short)
	return 0
}

// amend -----------------------------------------------------------

func (w *WitFuncs) runAmend(args []string, stdout, stderr io.Writer) int {
	// Parse: wit amend [-m MSG] <path> [<ref>]
	var message string
	var positional []string
	hasM := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-m":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "wit amend: -m requires a message")
				return 1
			}
			message = args[i+1]
			hasM = true
			i++
		default:
			if strings.HasPrefix(args[i], "-") {
				fmt.Fprintf(stderr, "wit amend: unknown flag %s\n", args[i])
				return 1
			}
			positional = append(positional, args[i])
		}
	}
	if len(positional) == 0 {
		fmt.Fprintln(stderr, "usage: wit amend [-m MSG] <path> [<ref>]")
		return 1
	}
	path := positional[0]
	ref := "HEAD"
	if len(positional) >= 2 {
		ref = positional[1]
	}
	kind, name, ok := requirePath(stderr, "amend", path)
	if !ok {
		return 1
	}
	if !hasM {
		fmt.Fprintln(stderr, "wit amend: -m <message> required")
		return 1
	}
	switch kind {
	case "bin":
		if err := w.Store.AmendShellFunctionMessage(w.CustomerID, name, ref, message); err != nil {
			if errors.Is(err, store.ErrRefNotFound) || errors.Is(err, store.ErrInvalidRef) ||
				errors.Is(err, store.ErrAmbiguousHash) {
				fmt.Fprintf(stderr, "wit amend: %v\n", err)
				return 1
			}
			fmt.Fprintf(stderr, "wit amend: %v\n", err)
			return 1
		}
	case "cron.d":
		if err := w.Store.AmendCronWatchMessage(w.CustomerID, name, ref, message); err != nil {
			fmt.Fprintf(stderr, "wit amend: %v\n", err)
			return 1
		}
	}
	return 0
}
