package coordinator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pmezard/go-difflib/difflib"
	"gorm.io/gorm"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/warp/pkg/syntax"
	"github.com/graybeardcorp/weft/store"
)

// WitFuncs holds the request-scoped state the `wit` builtin needs:
// the store handle, the active customer's ID, and the user driving
// the request. One per request — wired by the coordinator the same
// way WeftFuncs is.
type WitFuncs struct {
	Store  *store.Store
	OrgID  store.OrgID
	UserID string // user invoking; empty = system call (still works for read-only paths)
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
	versions, err := w.Store.LogShellFunction(w.OrgID, name)
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
			fmt.Fprint(stdout, colorDiff(formatVersionDiff(
				versions[i+1].Source, versions[i].Source, name,
				refLabel(i+1)+" "+versions[i+1].ContentHash[:7],
				refLabel(i)+" "+versions[i].ContentHash[:7])))
		}
	}
	return 0
}

func (w *WitFuncs) logCron(name string, patch bool, stdout, stderr io.Writer) int {
	versions, err := w.Store.LogCronWatch(w.OrgID, name)
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
			fmt.Fprint(stdout, colorDiff(formatVersionDiff(
				versions[i+1].Schedule+"\n"+versions[i+1].Pipeline,
				versions[i].Schedule+"\n"+versions[i].Pipeline, name,
				refLabel(i+1)+" "+versions[i+1].ContentHash[:7],
				refLabel(i)+" "+versions[i].ContentHash[:7])))
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
		v, err := w.Store.ResolveShellFunctionRef(w.OrgID, name, ref)
		if err != nil {
			fmt.Fprintf(stderr, "wit show: %v\n", err)
			return 1
		}
		content := v.Source
		fmt.Fprint(stdout, content)
		if !strings.HasSuffix(content, "\n") {
			fmt.Fprintln(stdout)
		}
	case "cron.d":
		cv, err := w.Store.ResolveCronWatchRef(w.OrgID, name, ref)
		if err != nil {
			fmt.Fprintf(stderr, "wit show: %v\n", err)
			return 1
		}
		schedule, pipeline := cv.Schedule, cv.Pipeline
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
		va, err := w.Store.ResolveShellFunctionRef(w.OrgID, name, aRef)
		if err != nil {
			fmt.Fprintf(stderr, "wit diff: %v\n", err)
			return 1
		}
		vb, err := w.Store.ResolveShellFunctionRef(w.OrgID, name, bRef)
		if err != nil {
			fmt.Fprintf(stderr, "wit diff: %v\n", err)
			return 1
		}
		fmt.Fprint(stdout, colorDiff(formatVersionDiff(
			va.Source, vb.Source, name,
			diffLabel(va.ContentHash, ownerLabel(w.Store, va.AuthoredByUserID)),
			diffLabel(vb.ContentHash, ownerLabel(w.Store, vb.AuthoredByUserID)))))
	case "cron.d":
		va, err := w.Store.ResolveCronWatchRef(w.OrgID, name, aRef)
		if err != nil {
			fmt.Fprintf(stderr, "wit diff: %v\n", err)
			return 1
		}
		vb, err := w.Store.ResolveCronWatchRef(w.OrgID, name, bRef)
		if err != nil {
			fmt.Fprintf(stderr, "wit diff: %v\n", err)
			return 1
		}
		fmt.Fprint(stdout, colorDiff(formatVersionDiff(
			va.Schedule+"\n"+va.Pipeline,
			vb.Schedule+"\n"+vb.Pipeline, name,
			diffLabel(va.ContentHash, ownerLabel(w.Store, va.AuthoredByUserID)),
			diffLabel(vb.ContentHash, ownerLabel(w.Store, vb.AuthoredByUserID)))))
	}
	return 0
}

// colorDiff wraps a unified-diff string with ANSI colors from
// warp's syntax package. Empty in → empty out (so callers that
// skipped a no-op diff section don't get a stray reset code).
//
// Color is unconditional today: wit output flows
// coordinator → SSE → shell, and by the time bytes reach a terminal
// the immediate writer at this call site can't see whether the
// destination is a TTY. The interactive shell — the only realistic
// consumer — renders the ANSI faithfully; tests substring-match
// through it. A user piping wit output to `patch(1)` would strip
// the codes (e.g. `sed 's/\x1b\[[0-9;]*m//g'`); we can grow a
// `--no-color` flag if anyone asks.
func colorDiff(s string) string {
	if s == "" {
		return ""
	}
	return syntax.Render(syntax.LangDiff, s)
}

// diffLabel formats the hash+owner annotation that goes into the
// tab-separated tail of `--- a/foo` and `+++ b/foo` lines.
// `patch(1)` ignores anything after the tab, so this is free real
// estate for human-readable provenance.
func diffLabel(hash, owner string) string {
	short := hash
	if len(hash) >= 7 {
		short = hash[:7]
	}
	return short + " " + owner
}

// formatVersionDiff renders a standard unified diff between two
// version contents, using `--- a/<name>` and `+++ b/<name>` headers
// with the supplied labels carried in the tab-separated tail (where
// `patch(1)` ignores them but `git diff` and IDE viewers display
// them). Three lines of context per hunk, multiple hunks when
// changes are far apart. Returns the empty string when contents
// are identical, so callers can decide whether to suppress the
// surrounding log entry / header section entirely.
//
// `oldLabel` and `newLabel` are free-form — `wit diff` passes
// hash+owner, `wit log -p` passes refLabel+hash. The trailing-newline
// quirk in difflib.SplitLines (it appends an extra empty line when
// input ends in `\n`) is normalized away here so callers don't have
// to worry about it.
func formatVersionDiff(oldContent, newContent, name, oldLabel, newLabel string) string {
	if oldContent == newContent {
		return ""
	}
	d := difflib.UnifiedDiff{
		A:        difflib.SplitLines(strings.TrimRight(oldContent, "\n")),
		B:        difflib.SplitLines(strings.TrimRight(newContent, "\n")),
		FromFile: "a/" + name,
		FromDate: oldLabel,
		ToFile:   "b/" + name,
		ToDate:   newLabel,
		Context:  3,
	}
	s, _ := difflib.GetUnifiedDiffString(d)
	return s
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
		v, err := w.Store.ResolveShellFunctionRef(w.OrgID, name, ref)
		if err != nil {
			fmt.Fprintf(stderr, "wit checkout: %v\n", err)
			return 1
		}
		if err := func() error {
			target, err := w.Store.ResolveShellFunctionRef(w.OrgID, name, ref)
			if err != nil {
				return err
			}
			if target.IsHead {
				return nil
			}
			return w.Store.DB().WithContext(store.WithOrg(context.Background(), w.OrgID)).Transaction(func(tx *gorm.DB) error {
				if err := tx.Exec("SELECT set_config('weft.acting_user_id', ?, true)", w.UserID).Error; err != nil {
					return err
				}
				updates := map[string]any{
					"source":       target.Source,
					"language":     target.Language,
					"content_hash": target.ContentHash,
					"message":      target.Message,
					"updated_at":   time.Now(),
				}
				if target.AuthoredByUserID != nil {
					updates["created_by_user_id"] = *target.AuthoredByUserID
				}
				return tx.Model(&store.ShellFunction{}).
					Where("name = ?", name).
					Updates(updates).Error
			})
		}(); err != nil {
			fmt.Fprintf(stderr, "wit checkout: %v\n", err)
			return 1
		}
		hash = v.ContentHash
	case "cron.d":
		v, err := w.Store.ResolveCronWatchRef(w.OrgID, name, ref)
		if err != nil {
			fmt.Fprintf(stderr, "wit checkout: %v\n", err)
			return 1
		}
		if err := func() error {
			target, err := w.Store.ResolveCronWatchRef(w.OrgID, name, ref)
			if err != nil {
				return err
			}
			if target.IsHead {
				return nil
			}
			compiled, err := store.CompileWatchPipeline(target.Pipeline)
			if err != nil {
				return err
			}
			return w.Store.DB().WithContext(store.WithOrg(context.Background(), w.OrgID)).Transaction(func(tx *gorm.DB) error {
				if err := tx.Exec("SELECT set_config('weft.acting_user_id', ?, true)", w.UserID).Error; err != nil {
					return err
				}
				updates := map[string]any{
					"schedule":        target.Schedule,
					"pipeline":        target.Pipeline,
					"content_hash":    target.ContentHash,
					"message":         target.Message,
					"alert_key":       compiled.AlertKey,
					"alert_tag":       compiled.AlertTag,
					"alert_condition": compiled.AlertCondition,
					"alert_threshold": compiled.AlertThreshold,
					"updated_at":      time.Now(),
				}
				if target.AuthoredByUserID != nil {
					updates["created_by_user_id"] = *target.AuthoredByUserID
				}
				return tx.Model(&store.CronWatch{}).
					Where("name = ?", name).
					Updates(updates).Error
			})
		}(); err != nil {
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
		if err := w.Store.AmendShellFunctionMessage(w.OrgID, name, ref, message); err != nil {
			if errors.Is(err, store.ErrRefNotFound) || errors.Is(err, store.ErrInvalidRef) ||
				errors.Is(err, store.ErrAmbiguousHash) {
				fmt.Fprintf(stderr, "wit amend: %v\n", err)
				return 1
			}
			fmt.Fprintf(stderr, "wit amend: %v\n", err)
			return 1
		}
	case "cron.d":
		if err := w.Store.AmendCronWatchMessage(w.OrgID, name, ref, message); err != nil {
			fmt.Fprintf(stderr, "wit amend: %v\n", err)
			return 1
		}
	}
	return 0
}
