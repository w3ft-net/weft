// Package onboarding holds the lesson definitions for the Loom Inc
// onboarding tour. Lessons are evaluated by api/shell.go after every
// successful pipeline execute: when the just-run pipeline matches the
// current lesson's Action verbatim, the step advances and the next
// lesson's prompt copy is emitted as a special SSE event for the
// frontend to render (and as ANSI text for the CLI).
//
// Matching is exact (modulo surrounding whitespace) against Action —
// the same string the web shell injects into the input buffer. So as
// long as the user hits enter on what we suggested, they advance; if
// they edit it, they don't. The tour overlay exposes a re-paste
// affordance for users who wandered off.
package onboarding

import (
	"regexp"
	"strings"
)

// Lesson is one step of the shell-* onboarding stage. The frontend
// (and CLI) renders Prompt before the user attempts the lesson, and
// the web shell pre-injects Action into the input. Running Action
// (verbatim) advances the step.
type Lesson struct {
	Step   string
	Action string
	Prompt string
	// ActionPattern, when set, makes CheckPipeline accept any pipeline
	// matching this compiled regex instead of requiring an exact match
	// against Action. For a lesson whose command embeds a value the
	// user must discover from their own prior output (e.g. a real
	// trace_id) rather than a fixed literal every user shares. Action
	// still holds an illustrative example, shown as the reference/
	// re-paste text — it will not itself match ActionPattern, which is
	// intentional: re-pasting it unedited should not silently "succeed"
	// without the user substituting a real discovered value.
	ActionPattern *regexp.Regexp
	// CompletesOnInterrupt marks a lesson whose command is a live stream
	// the user ends with Ctrl+C (exit 130). For these, the interrupt IS the
	// completion rather than a failure to finish.
	CompletesOnInterrupt bool
	// ClosesWithModal marks a lesson whose command opens a modal over the
	// shell. The command finishing does not complete the lesson: advancing
	// would prefill the next command beneath the open modal. The lesson
	// completes when the browser reports the modal closed.
	ClosesWithModal bool
}

// Trigger is what the server observed that might complete a lesson.
type Trigger int

const (
	// TriggerExec: the server's exec path saw the pipeline finish, from a
	// client that renders its own output (the CLI).
	TriggerExec Trigger = iota
	// TriggerBrowserDone: the browser shell reported the line completed.
	TriggerBrowserDone
	// TriggerModalFetch: the exec path saw a pipeline a browser modal ran to
	// fetch its own rows.
	TriggerModalFetch
	// TriggerModalClosed: the browser reported the user dismissed the modal.
	TriggerModalClosed
)

// CompletesOn reports whether trigger completes the lesson at step. A lesson
// that ClosesWithModal completes on the modal closing — or on a plain exec,
// since a client with no modal has nothing else to wait for. Any other
// lesson completes with its command and ignores modal closure.
func CompletesOn(step string, trigger Trigger) bool {
	lesson := Lookup(step)
	if lesson == nil {
		return false
	}
	if lesson.ClosesWithModal {
		return trigger == TriggerModalClosed || trigger == TriggerExec
	}
	return trigger != TriggerModalClosed
}

// exitInterrupted is the exit status the shell reports when the user stops
// a running command with Ctrl+C (128 + SIGINT).
const exitInterrupted = 130

// AdvancesOnExit reports whether a completed line with the given exit code
// may advance the lesson at step. A clean exit does; an interrupt does only
// for a lesson that CompletesOnInterrupt; any other failure never does.
func AdvancesOnExit(step string, exitCode int) bool {
	lesson := Lookup(step)
	if lesson == nil {
		return false
	}
	switch exitCode {
	case 0:
		return true
	case exitInterrupted:
		return lesson.CompletesOnInterrupt
	}
	return false
}

// traceIDPipelinePattern matches `trace --id <id>` where <id> is exactly
// the shape weft-go's NewTraceID() produces: 32 lowercase hex chars.
var traceIDPipelinePattern = regexp.MustCompile(`^trace --id [0-9a-f]{32}$`)

// lessons is the canonical, ordered list. The first 4 entries ship
// with the initial onboarding cut; the rest are placeholders the
// step state machine knows about (so api/onboarding.go's
// nextOnboardingStep walks past them) and can be filled in as
// follow-ups.
var lessons = []*Lesson{
	{
		Step:   "shell-ls",
		Prompt: "Now we are into the heart of weft -- the shell. In short, it's the subset of bash we find most useful for working with logs, made to work with distributed cloud storage. Plus a few special builtin commands. Hit enter to start looking around.",
		Action: "ls /log",
	},
	{
		Step:   "shell-ls-loom",
		Prompt: "`loom` here is the only namespace the org has set up. If you had additional namespaces, they would be sibling directories. Run `tree` to see everything under /log/loom.",
		Action: "tree /log/loom",
	},
	{
		Step:   "shell-range",
		Prompt: "Log paths take the format `/log/<namespace>/(<env>/)?<service>/<kind>.log`. The env is optionally embedded in your tokens. The `service` and `kind` segment is either set explicitly in your weftd config or assigned automatically for other producer types. In this case, `main.log` is stdout on our hosts, and `metrics.log`, are telemetry coming from the weft SDK.\n\nNow have a look at your PS1. See that `[1d]`? That's indicating the time range of log lines that will be returned when you hit one of your aggregate log files. Try changing it: you can run `range 1h..` to set it to the past hour.",
		Action: "range 1h..",
	},
	{
		Step:   "shell-tail",
		Action: "tail /log/loom/prod/api/main.log",
		Prompt: "Now tail some logs to see live API request lines flowing in. What's cooking on those Loom servers?",
	},
	{
		Step:                 "shell-tail-f",
		CompletesOnInterrupt: true,
		Action:               "tail -f /log/loom/prod/api/main.log",
		Prompt:               "`tail` is nice but half the reason we built weft was because we missed a good old fashioned `tail -f`. Try following the logs -- it's pretty fast! Ctrl-C when you're ready to move on.",
	},
	{
		Step:   "shell-cd",
		Action: "cd /log/loom/prod/api",
		Prompt: "Normal shell directory commands apply here. `cd` into a service's directory to type less (e.g. `tail main.log` instead of the full path).",
	},
	{
		Step:   "shell-grep-awk",
		Action: "tail main.log | grep ERROR | awk '{print HOST, $1, $5}'",
		Prompt: "So obviously we have support for awk and grep, but the more material point is `HOST` in the awk program. Weft augments normal field variables in awk and jq with a metavariables to help you isolate exactly where those log lines were produced. More info in the manual.",
	},
	{
		Step:   "shell-trace-find",
		Action: "tail main.log | grep ' checkout ' | awk '{print TRACE_ID}'",
		Prompt: "Once such metavariable that you can supply is trace ids, and Loom has them configured. Their checkout flow runs from API to an edge payment gateway to a background fulfillment worker. Each hop is tagged with the same trace_id. Let's dump some to stdout.",
	},
	{
		Step:            "shell-trace",
		Action:          "trace --id $REPLACE_ME",
		ActionPattern:   traceIDPipelinePattern,
		ClosesWithModal: true,
		Prompt:          "Let's run the `trace` command! Grab a trace id from the previous output and pass it to `trace` with `--id`. Press Esc to close the trace view when you're done.",
	},
	{
		Step:   "shell-alert",
		Action: "tail main.log | grep ERROR | wc -l | alert above 0",
		Prompt: "Here's another special builtin: `alert`. For now, this is just going to log to stderr, but you can get all sorts of alerting targets on a cron (more info shortly). The api/prod stream emits errors at ~8% so this fires every time.",
	},
	{
		Step:   "shell-notify",
		Prompt: "When you aren't just playing around in the shell and you want alerts to go somewhere real, you create a mapping in `/etc/notify.yaml`. You can use the top level keys as aliased arguments to `alert` so you can have different behaviors for different alerts. ",
		Action: "cat /etc/notify.yaml",
	},
	{
		Step:   "shell-cron",
		Action: "ls -l /cron.d/",
		Prompt: "When you want to monitor the output of a one-liner, you just stick a file in /cron.d/. Loom has a couple examples for you.",
	},
	{
		Step:   "shell-schedule",
		Action: "cat /cron.d/error-rate",
		Prompt: "Crons are just scripts (usually bash) with a leading comment that contains a schedule either in OG cron format or 'every n<interval>'. This one just runs every minute, looking for >10% bursts of error log lines.",
	},
	{
		Step:   "shell-bin-bash",
		Action: "ls /bin/",
		Prompt: "If your monitoring patterns need more than a one-liner or you want to parameterize, just add a script to /bin/. These are available to your entire organization. Bash and lua are the supported languages.",
	},
	{
		// Step ID is historical ("shell-wit"): the lesson used to teach the
		// `wit` command, which `git` replaced. Kept so recorded progress holds.
		Step:   "shell-wit",
		Action: "git log -p -- /bin/scan",
		Prompt: "Bin scripts, cron entries, and /etc have version history, which you can access using git commands (a faked, limited subset just to accommodate your muscle memory). `git blame` also supported. Run `git --help` if you want to see everything available.",
	},
	{
		Step:   "shell-bin-lua",
		Action: "vim /bin/severity",
		Prompt: "Look at this, it's a Lua file! Weft will respect the shebang like we have here, or a .lua extension.\n\nJust like git, weft's vim is an illusion, so not everything here is going to work, but it's a pretty decent subset.\n\nNow that we're here, make a few edits to the file and try to save. You'll see that you haven't been granted full permissions in the Loom org so your write access is restricted. Then you can `:q!` to quit.",
	},
	{
		Step:   "shell-tmp",
		Action: "echo hello > /tmp/note",
		Prompt: "If you are working through an issue where it's easier to stage output from some one-liners and run `comm` or `join` on those files, dump them to /tmp. But beware -- because scratch files tend to abide by gas laws and fill the size of the container, we automatically delete files that haven't been written to for 48 hrs.",
	},
	{
		Step:   "shell-clipboard",
		Action: "clipboard /bin/scan",
		Prompt: "`clipboard` builds a deep link that captures the script path, your current range, and any args, and sticks it on your system clipboard. Useful for a teammate that won't jump into a tmux with you.",
	},
	{
		Step:   "shell-weftfunc",
		Action: "weft::avg error-rate 1h",
		Prompt: "Sooooo you have some scripts running in cron.d. Weft has some convenience functions for aggregation, namespaced under `weft::<fn>` in bash (or `weft.<fn>(...)` in lua). Most take a duration. There's also a parallel family of predicates like `weft::is_above` and `weft::is_outlier`. These compose naturally into alerts, e.g. `weft::avg error-rate 1m | alert above 100`. Full reference: `man weft`.",
	},
	{
		Step:            "shell-chart",
		Action:          "chart error-rate",
		Prompt:          "`chart` is another weft-specific builtin. It'll open a modal in the browser. The CLI will render right in the terminal, though not quite as pretty. Close the chart when you're done to keep on the tour.",
		ClosesWithModal: true,
	},
	{
		Step:            "shell-chart-pipe",
		Action:          "cat main.log | grep ERROR | chart -x 1 --bucket 5m",
		Prompt:          "`chart` doesn't need a saved cron behind it — pipe any pipeline straight into it and chart it directly, bucketed however you want. `-x 1` tells it to bucket by the timestamp in column 1; with no `-y`, it counts rows per bucket.",
		ClosesWithModal: true,
	},
	{
		Step:   "shell-dashboard-yaml",
		Action: "cat /etc/dashboards/prod.yaml",
		Prompt: "You can composite charts into dashboards with a bit YAML. Like with /etc/notify.yaml, the schema will be validated on write.",
	},
	{
		Step:   "shell-dashboard",
		Action: "dashboard prod",
		Prompt: "Just like with `chart`, the `dashboard` builtin works in the terminal, but will rely on more modern rendering tools when in the browser.",
	},
	{
		// Step ID is historical ("team-yaml"); the lesson now teaches the
		// read-only /etc/passwd roster + group model that replaced it.
		Step:   "shell-team-yaml",
		Action: "cat /etc/passwd",
		Prompt: "`/etc/passwd` is a read-only roster for whatever org you have active. /bin, /cron.d, and /etc all have corresponding org-level edit perms (`binmgr`/`cronmgr`/`configmgr`). Read perms for logs -- see how you have `loom` perms? -- and ability to mint tokens like `loom+tokens` are by namespace. Org owners manage privs either with the weft CLI or the Members page of the web dashboard.",
	},
	{
		Step:   "shell-usage",
		Action: "cat /etc/usage",
		Prompt: "`/etc/usage` shows you weft's tally of bytes ingested per stream.",
	},
}

// Steps returns the ordered list of shell-* step keys defined by
// the lesson array. This is the canonical source for which shell
// lessons the state machine knows about — api/onboarding.go folds
// these into its full sequence (between the non-shell prefix and
// the "done" terminator) so renaming or reordering a lesson here
// automatically updates the advance state machine. Adding a lesson
// to lessons[] and forgetting to update the api-side sequence is
// the brittleness this exists to eliminate.
func Steps() []string {
	out := make([]string, 0, len(lessons))
	for _, l := range lessons {
		out = append(out, l.Step)
	}
	return out
}

// Lookup returns the lesson definition for a given onboarding step,
// or nil if the step isn't a shell-* lesson. Callers in the shell
// path use this to decide whether to even attempt predicate
// matching (most commands won't be in onboarding mode at all).
func Lookup(step string) *Lesson {
	for _, l := range lessons {
		if l.Step == step {
			return l
		}
	}
	return nil
}

// CheckPipeline returns true when the given pipeline matches the
// current lesson. Most lessons match the Action string verbatim
// (after trimming surrounding whitespace) — the web shell injects
// Action into the input buffer, so a user who just hits enter
// advances; any edit they make breaks the match. A lesson with
// ActionPattern set instead accepts any pipeline matching that regex,
// for commands that embed a value the user must discover themselves
// (e.g. a real trace_id) rather than share one fixed literal.
func CheckPipeline(currentStep, pipeline string) bool {
	lesson := Lookup(currentStep)
	if lesson == nil {
		return false
	}
	trimmed := strings.TrimSpace(pipeline)
	if lesson.ActionPattern != nil {
		return lesson.ActionPattern.MatchString(trimmed)
	}
	if lesson.Action == "" {
		return false
	}
	return trimmed == strings.TrimSpace(lesson.Action)
}

// PromptFor returns the rendered prompt copy for a step, or "" if
// the step has no lesson definition. Used by the CLI / web shell to
// surface the next instruction after a successful advance.
func PromptFor(step string) string {
	lesson := Lookup(step)
	if lesson == nil {
		return ""
	}
	return lesson.Prompt
}

// ActionFor returns the lesson's suggested command for a step, or ""
// if the step has no lesson definition. The web shell injects this
// into the input buffer so the user can just hit enter to advance.
func ActionFor(step string) string {
	lesson := Lookup(step)
	if lesson == nil {
		return ""
	}
	return lesson.Action
}
