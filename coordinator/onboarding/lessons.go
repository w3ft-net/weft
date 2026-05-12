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

import "strings"

// Lesson is one step of the shell-* onboarding stage. The frontend
// (and CLI) renders Prompt before the user attempts the lesson, and
// the web shell pre-injects Action into the input. Running Action
// (verbatim) advances the step.
type Lesson struct {
	Step   string
	Action string
	Prompt string
}

// lessons is the canonical, ordered list. The first 4 entries ship
// with the initial onboarding cut; the rest are placeholders the
// step state machine knows about (so api/onboarding.go's
// nextOnboardingStep walks past them) and can be filled in as
// follow-ups.
var lessons = []*Lesson{
	{
		Step:   "shell-ls",
		Prompt: "Now we are into the meat of weft -- the shell. A lot will be familiar, and a few things are particular to weft. Let's get started. Hit enter to see what is kicking about in the weft shell.",
		Action: "ls /log",
	},
	{
		Step:   "shell-ls-loom",
		Prompt: "`loom` here is our sole defined namespace. If you had additional namespaces, they would be sibling directories. Now `ls` again to see what's in /log/loom.",
		Action: "tree /log/loom",
	},
	{
		Step:   "shell-range",
		Prompt: "You can label your access tokens with an environment, and if you do, the next layer down in the directory structure will be the env. Services self-report a name via their integration, so that becomes the log file. If you hae metrics enabled, that will fall under <service>/metrics.log.\n\nNow have a look at your PS1. See that `[1d]`? That's indicating the time range of log lines that will be returned when you hit one of your aggregate log files. Try changing it: you can run `range 1h..` to set it to the past hour.",
		Action: "range 1h..",
	},
	{
		Step:   "shell-tail",
		Action: "tail /log/loom/prod/api.log",
		Prompt: "Now tail some logs to see live API request lines flowing in. (Loom Inc might be a fake company but they have real servers.)",
	},
	{
		Step:   "shell-grep-awk",
		Action: "cat /log/loom/prod/api.log | grep ERROR | awk '{print $HOST, $1, $4}'",
		Prompt: "So obviously we have support for awk and grep, but the more material point is `$HOST` in the awk program. Weft augments normal field variables in awk and jq with a number of meta-variables to help you isolate exactly where those log lines were produced. More info in the manual.",
	},
	{
		Step:   "shell-alert",
		Action: "cat /log/loom/prod/api.log | grep ERROR | wc -l | alert above 0",
		Prompt: "Weft contains a special builtin called alert. For now, this is just going to log to stderr, but you can get all sorts of alerting targets on a cron (more info shortly). The api/prod stream emits errors at ~8% so this fires every time.",
	},
	{
		Step:   "shell-notify",
		Prompt: "/etc/notify.yaml maps alert aliases to where they go (webhooks, email, Slack, PagerDuty). There is no GUI where you can configure this. Like nearly everything in weft, you do it in the shell.",
		Action: "cat /etc/notify.yaml",
	},
	{
		Step:   "shell-cron",
		Action: "ls /cron.d/",
		Prompt: "When you want to monitor the output of a one-liner, you just stick a file in /cron.d/. `cat` one of the examples here. Each cron maintains timeseries data that can be aggregated and charted.",
	},
	{
		Step:   "shell-schedule",
		Action: "cat /cron.d/error-rate",
		Prompt: "Crons are just bash scripts with a leading comment that contains a schedule either in OG cron format or 'every n<interval>'.",
	},
	{
		Step:   "shell-bin-bash",
		Action: "ls /bin/",
		Prompt: "If your monitoring patterns need more than a one-liner or you want to parameterize, just add a script to /bin/. These are available to your entire organization. Bash and lua are the supported languages.",
	},
	{
		Step:   "shell-wit",
		Action: "wit log /bin/scan",
		Prompt: "The weft shell includes a lightweight version history tool for bin scripts and cron entries called `wit`. The name might not be clever, but you'll remember exactly what it's for!  History tracking is automatic and users are prompted for commit messages in vim whenever they create or modify a file.",
	},
	{
		Step:   "shell-bin-lua",
		Action: "vim /bin/severity",
		Prompt: "Without a shebang, weft assumes your extensionless scripts are bash. You don't need the shebang for lua if you add the extension.\n\nNow that we're in vim, make a few edits to the file and try to save. You'll see that you haven't been granted full permissions in the Loom org so your write access is restricted. Then you can `:q!` to quit.",
	},
	{
		Step:   "shell-clipboard",
		Action: "clipboard /bin/scan",
		Prompt: "When a /bin/ script is worth showing to a teammate, `clipboard` builds a deep link that captures the script path, your current range, and any args, and pushes it onto your system clipboard via OSC-52. Anyone signed in to the same org can paste the URL into a browser to land in the shell with the invocation pre-filled. More in `man clipboard`.",
	},
	{
		Step:   "shell-team-yaml",
		Action: "cat /etc/team.yaml",
		Prompt: "The reason that save failed: /etc/team.yaml is the org's permissions file. Owners list each non-owner member here with `edit` or `read-only` against /bin, /cron.d, and /etc, plus a per-namespace block. Owners don't appear — they have implicit full access. The file is editable like any other (`vi /etc/team.yaml`), but writes are owner-only no matter what permissions the editor has elsewhere. The Members tab in the dashboard reads and writes the same data through a checkbox grid.",
	},
	{
		Step:   "shell-weftfunc",
		Action: "weft.avg error-rate 1h",
		Prompt: "Once you have some scripts running in cron.d, there are some convenience functions for aggregations namespaced under `weft.<fn>`, exposed both in bash and in lua. Most of these take a duration (`weft.latest` does not). These compose naturally into alerts, e.g. `weft.avg error-rate 1m | alert above 100`. The full list of functions available is in the manual.",
	},
	{
		Step:   "shell-tmp",
		Action: "echo hello > /tmp/note; cat /tmp/note",
		Prompt: "If you are working through an issue where it's easier to stage output from some one-liners and run `comm` or `join` on those files, dump them to /tmp. Because scratch files tend to abide by gas laws and fill the size of the container, we automatically delete files that haven't been written to for 48 hrs.",
	},
	{
		Step:   "chart",
		Action: "chart error-rate",
		Prompt: "`chart` is another weft-specific builtin. It'll open a modal in the browser. The CLI will render right in the terminal with as much fidelity as it can.",
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
		Step:   "shell-usage",
		Action: "what costs you what",
		Prompt: "Try `cat /etc/usage` — weft's tally of bytes ingested per stream, and what you'd be billed at the current tier.",
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

// CheckPipeline returns true when the given pipeline is the lesson's
// Action verbatim (after trimming surrounding whitespace). The web
// shell injects Action into the input buffer, so a user who just
// hits enter advances; any edit they make breaks the match.
func CheckPipeline(currentStep, pipeline string) bool {
	lesson := Lookup(currentStep)
	if lesson == nil || lesson.Action == "" {
		return false
	}
	return strings.TrimSpace(pipeline) == strings.TrimSpace(lesson.Action)
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
