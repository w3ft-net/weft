// Package onboarding holds the lesson definitions for the Loom Inc
// onboarding tour. Lessons are evaluated by api/shell.go after every
// successful pipeline execute: when the just-run pipeline matches the
// current lesson's predicate, the step advances and the next lesson's
// prompt copy is emitted as a special SSE event for the frontend to
// render (and as ANSI text for the CLI).
//
// Predicates intentionally use string/regex matching against the
// pipeline source rather than parsed-AST inspection. Two reasons:
// (1) it lets us trigger on any reasonable user variation (`cat`
// vs `tail`, with or without flags) without enumerating every AST
// shape; (2) it keeps the dependency surface small — onboarding
// doesn't need to import warp's parse tree.
package onboarding

import (
	"regexp"
	"strings"
)

// Lesson is one step of the shell-* onboarding stage. The frontend
// (and CLI) renders Prompt before the user attempts the lesson; if
// the user's pipeline matches Predicate, the step advances and the
// next lesson's Prompt gets surfaced.
//
// Hint is a follow-up nudge surfaced when the user's input is
// close-but-wrong — same predicate it'd take to advance, but
// missing one piece. The MVP doesn't fire it (every non-match is
// silent); the field's there so we can layer it in without changing
// the type later.
type Lesson struct {
	Step      string
	Title     string
	Prompt    string
	Hint      string
	Predicate func(pipeline string) bool
}

// matches builds a Predicate that requires every pattern to match
// the pipeline (case-insensitive substring or compiled regex). Used
// by the lesson list below to keep the per-step boilerplate small.
func matches(patterns ...string) func(string) bool {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		compiled = append(compiled, regexp.MustCompile("(?i)"+p))
	}
	return func(pipeline string) bool {
		for _, re := range compiled {
			if !re.MatchString(pipeline) {
				return false
			}
		}
		return true
	}
}

// lessons is the canonical, ordered list. The first 4 entries ship
// with the initial onboarding cut; the rest are placeholders the
// step state machine knows about (so api/onboarding.go's
// nextOnboardingStep walks past them) and can be filled in as
// follow-ups.
var lessons = []*Lesson{
	{
		Step:   "shell-ls",
		Title:  "see what's there",
		Prompt: "Try `ls /log` to see Loom's log streams. Each stream is one app emitting lines into weft.",
		Hint:   "weft pseudofiles live under /log/<customer>/<env>/<app>.log — `ls /log` lists them.",
		// Match `ls` with /log somewhere in the pipeline. Allow
		// `ls /log/`, `ls -la /log`, etc.
		Predicate: matches(`\bls\b`, `/log`),
	},
	{
		Step:   "shell-cat",
		Title:  "read a stream",
		Prompt: "Now `cat /log/loom/prod/api.log` to see live API request lines flowing in.",
		Hint:   "cat reads the whole window; tail -n N is the easier read for streams that are flowing fast.",
		Predicate: matches(`\b(cat|tail)\b`, `/log/loom`),
	},
	{
		Step:   "shell-grep-awk",
		Title:  "filter and shape",
		Prompt: "Pipe through grep and awk: `cat /log/loom/prod/api.log | grep ERROR | awk '{print $1, $4}'`. Find the errors and pull just the timestamp + status.",
		Hint:   "grep and awk are real Unix commands here — anything you'd pipe in bash works.",
		Predicate: matches(`\b(cat|tail)\b`, `/log/loom`, `\|.*grep`, `\|.*awk`),
	},
	{
		Step:   "shell-alert",
		Title:  "trigger an alert",
		Prompt: "Pipe an error count into `alert above 0`: `cat /log/loom/prod/api.log | grep ERROR | wc -l | alert above 0`. The api/prod stream emits errors at ~8% so this fires every time.",
		Hint:   "alert reads a numeric value off stdin and fires when it crosses the threshold. Try `alert above 0` first; it's the easiest to verify.",
		Predicate: matches(`/log/loom`, `\balert\b`, `\babove\b`),
	},
	// shell-notify, shell-cron, shell-weftfunc, shell-bin-bash,
	// shell-bin-lua, shell-usage, shell-tmp, shell-wit,
	// shell-dashboard-yaml — placeholders. They live in the step
	// list (api/onboarding.go's onboardingSequence) so advancing
	// past shell-alert lands here in order. Nil Predicate means the
	// shell-side advance always falls back to client-driven advance
	// via /api/onboarding/advance, which is the right behavior
	// while we build out the rest.
	{Step: "shell-notify", Title: "notify routing", Prompt: "/etc/notify.conf maps alert names to where they go (Slack, PagerDuty, stdout). Try `cat /etc/notify.conf` to see Loom's setup."},
	{Step: "shell-cron", Title: "scheduled pipelines", Prompt: "Loom has cron entries in /cron.d/. Try `ls /cron.d` to see them. Each cron run produces a row of timeseries data you can chart."},
	{Step: "shell-weftfunc", Title: "weft aggregations", Prompt: "Try `cat /log/loom/prod/api.log | weft.count` — weft.* commands aggregate stream values without round-tripping to a separate query."},
	{Step: "shell-bin-bash", Title: "bash bin/ scripts", Prompt: "Loom has saved scripts in /bin/. Try `cat /bin/scan` — it's a bash function defined inside weft."},
	{Step: "shell-bin-lua", Title: "lua bin/ scripts", Prompt: "Some /bin/ scripts are written in lua for richer control flow. Try `ls /bin/` to find them."},
	{Step: "shell-usage", Title: "what costs you what", Prompt: "Try `cat /etc/usage` — weft's tally of bytes ingested per stream, and what you'd be billed at the current tier."},
	{Step: "shell-tmp", Title: "scratch space", Prompt: "Try `echo hello > /tmp/note` then `cat /tmp/note`. /tmp is a real per-org filesystem you can use for scratch work."},
	{Step: "shell-wit", Title: "version history", Prompt: "Every /bin/ and /cron.d/ entry is versioned. Try `wit log /bin/scan` to see who changed it and when."},
	{Step: "shell-dashboard-yaml", Title: "dashboards as YAML", Prompt: "Dashboards are YAML files. Try `cat /dashboards/error-rate.yaml` to see how Loom's main dashboard is defined."},
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

// CheckPipeline returns true when the given pipeline satisfies the
// current step's predicate. False for steps without a Predicate
// (placeholder lessons or non-shell steps).
func CheckPipeline(currentStep, pipeline string) bool {
	lesson := Lookup(currentStep)
	if lesson == nil || lesson.Predicate == nil {
		return false
	}
	// Trim shellcore bookkeeping comments / leading whitespace before
	// matching so a pipeline indented in a heredoc still triggers.
	return lesson.Predicate(strings.TrimSpace(pipeline))
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
