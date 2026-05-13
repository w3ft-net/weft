# Plan: onboarding via the Loom Inc demo org

## Status

Shipped. The five PRs sketched at the bottom all landed: Loom demo
seed + producers, signup-time Loom membership grant, welcome modal,
panel tour, instrumented shell with lesson hooks, and graduation
(membership revocation + home-org redirect + one-shot success
banner). See `coordinator/onboarding/lessons.go`,
`api/onboarding.go`, and `frontend/src/components/{TourOverlay,
WelcomeModal,GraduationBanner}.tsx`.

## Why

Brand-new signups currently land in an empty tenant with no logs,
no crons, no dashboards, and a vague pointer at the docs. The
shell's distinguishing features (pseudofiles, time meta-vars,
cron-as-script, wit, notify.conf, /etc/usage) are exactly the
shape that's hardest to discover by reading.

The fix is to give every signup a second membership in a fully
populated demo org — Loom Inc — that has real producers feeding
real shards, real cron history, real bin/ scripts, real
dashboards. Onboarding is then a guided tour of *that* tenant,
read-only. Once the user finishes, we revoke the membership and
leave them with a banner pointing at the manual.

The user keeps their own (still-empty) org for actual data;
nothing about onboarding pollutes it. Failure mode is "user did
nothing in their own org for 24h" — same as today.

## Architecture

### Storage

```sql
ALTER TABLE users
  ADD COLUMN onboarding_step       TEXT  NOT NULL DEFAULT 'welcome',
  ADD COLUMN onboarding_dismissed  BOOL  NOT NULL DEFAULT FALSE;
```

`onboarding_step` is a text enum:

```
welcome → dashboards → namespaces → downloads →
  shell-ls → shell-cat → shell-grep-awk → shell-alert →
  shell-notify → shell-cron → shell-weftfunc →
  shell-bin-bash → shell-bin-lua → shell-usage →
  shell-tmp → shell-wit → shell-dashboard-yaml →
  done
```

Server-side state lives on the user record so it survives device
swap (web → CLI → mobile). `onboarding_dismissed` is the explicit
bail-out — once true, no overlays anywhere.

### Membership lifecycle

- **Signup:** insert TeamMember row in Loom Inc, role=member,
  status=active. Insert NamespacePermission(CanRead=true) on Loom's
  demo namespace. Set `users.last_active_customer_id` to the user's
  *own* org so they don't auto-land in Loom.
- **Tour:** the user explicitly switches to Loom (modal nudge), at
  which point everything they see is Loom data, scoped read-only by
  their NamespacePermission grant.
- **Graduation:** `onboarding_step → done` in the API → server
  deletes the Loom TeamMember row + NamespacePermission, sets
  `last_active_customer_id` back to the user's own org. Next page
  load: org switcher hides Loom, banner shows "now set up your own
  namespaces — everything's in the manual."

Membership revocation is the boundary, not a runtime check. After
graduation, the user's JWT still validates, but `BuildSession`
finds no Loom membership and any X-Active-Org=loom request gets a
clean 403. No special-cased "are they a graduate" branch in any
handler.

### The four UI stages

#### Stage 1 — welcome modal (web only)

Renders when `user.onboarding_step == 'welcome'` and
`onboarding_dismissed == false`.

Copy: "Welcome to weft! To help you learn weft at warp-speed,
we've made you a member of Loom Inc — a fake org we run with
realistic logs flowing through it. Switch to Loom in the org
dropdown to start a quick walkthrough."

Buttons: `[Start the tour]` (auto-switches to Loom and advances
step → dashboards), `[Skip]` (sets dismissed=true).

The modal is one component, hangs off the `AuthProvider` so it
renders above any page.

#### Stage 2 — panel tour (web only)

A `TourOverlay` component that's always mounted at the layout
root. It reads `user.onboarding_step`; when the step matches a
panel-tour step, it renders an overlay anchored to the relevant
sidebar item.

Panel sequence:

| Step          | Sidebar target | Copy beat                                 |
|---------------|---------------|--------------------------------------------|
| `dashboards`  | Dashboards    | "Live charts of every metric Loom emits."  |
| `namespaces`  | Namespaces    | "Each app's logs live in its own namespace." |
| `downloads`   | Downloads     | "Grab the CLI / weftd to ship your own logs." |

Advance trigger: clicking the highlighted sidebar item navigates
to the route AND advances `onboarding_step` via
`POST /api/onboarding/advance`. A `[Next]` button in the overlay
is the fallback for anyone who'd rather not click the highlight.

After `downloads`, the next step is `shell-ls` and the overlay
copy says "now let's drop into the shell" with a single button
that navigates to /shell.

#### Stage 3 — instrumented shell

This is the meatiest stage. Two coordinated layers:

**Server-side: `OnboardingHooks`** — a new
`shellcore.OnboardingHooks` analogous to the existing `VimHooks`.
The shell session installs these hooks only when:

- `session.ActiveCustomerName == "loom"` AND
- `user.onboarding_step` is in the shell-* range AND
- `user.onboarding_dismissed == false`

The hooks fire at three points:

- `OnParse(pipeline ast)` — inspect the just-parsed pipeline
  *before* execute. If the user typed something close-but-not-
  quite, render an inline hint: "looks like you're trying to
  filter — try `cat foo.log | grep ERROR`." No disruption to
  execution.
- `OnExecuteOK(pipeline, result)` — pipeline ran successfully.
  Check `success_predicate` for the current lesson; if it passes,
  call `s.store.AdvanceOnboardingStep(userID, nextStep)`, render
  ANSI lines above the next prompt: `✓ nice. now try: tail -f
  /log/loom/payments.log`.
- `OnExecuteErr(pipeline, err)` — pipeline failed. If the failure
  is in the spirit of the lesson (e.g. trying to write while
  read-only), surface a teaching moment: "in your own org, this
  would queue a proposal."

Each lesson is a value, not code:

```go
type Lesson struct {
    Step             string
    Prompt           string
    Hint             string
    SuccessPredicate func(parsed *ast.Pipeline, result *ExecResult) bool
}
```

Lessons live as a flat slice in a new `coordinator/onboarding/`
package. Don't build a YAML loader yet — the predicates need
real Go to look at parsed pipelines.

**Client-side: `TourOverlay` continues** — the same React
component that drove the panel tour reads `onboarding_step`. For
shell-* steps:

- Render a small banner above the `xterm` container with the
  current lesson's prompt copy.
- When the step matches a shell-chrome button (e.g. `shell-cron`
  highlights the watches button, `shell-bin-bash` highlights the
  functions button), pin a `data-onboarding-highlight` attr on
  that button and pulse it.
- The web view polls `/api/auth/me` every few seconds to pick up
  `onboarding_step` advances pushed by the server-side hook —
  there's no need for a websocket here.

Lesson sequence:

| Step                   | Prompt                                      | Highlight  |
|------------------------|---------------------------------------------|------------|
| `shell-ls`             | run `ls /log` to see Loom's streams         | —          |
| `shell-cat`            | `cat /log/loom/api.log` — the basics        | —          |
| `shell-grep-awk`       | filter for ERROR with grep, then awk        | —          |
| `shell-alert`          | trigger a known-failing condition; alert    | —          |
| `shell-notify`         | `cat /etc/notify.conf` — alert routing      | —          |
| `shell-cron`           | inspect a cron entry; explain timeseries    | watches btn|
| `shell-weftfunc`       | run a pipeline that uses a weftfunc         | —          |
| `shell-bin-bash`       | look at a bash bin/ script                  | functions btn |
| `shell-bin-lua`        | look at a lua bin/ script                   | functions btn |
| `shell-usage`          | `cat /etc/usage` — what costs you what      | —          |
| `shell-tmp`            | scratch work in a /tmp file                 | —          |
| `shell-wit`            | `wit log /bin/some-script` — version history | —         |
| `shell-dashboard-yaml` | view a yaml dashboard def                   | —          |

When a step has a highlight target, the lesson's prompt copy
explicitly mentions both pathways: "click the watches button at
the top OR type `cat /cron.d/error-rate`." The button is the
phone-friendly path.

Read-only as teaching moment: any step where the natural urge is
to write (`vim /bin/foo`, `wit commit`) should *let the user try*,
let it fail with the existing forbidden-error path, and then
intercept the error to render a friendly teaching line. The
forbidden-error path is already structural (NamespacePermission);
the onboarding hook only adds the rendering.

#### Stage 4 — graduation

Triggered when `OnExecuteOK` advances the step from
`shell-dashboard-yaml` to `done`. Server-side immediately:

1. Delete the user's TeamMember row in Loom.
2. Delete the NamespacePermission rows on Loom's demo namespace.
3. Set `users.last_active_customer_id` to the user's own org.
4. Insert a one-shot user_notice (`onboarding_graduated`).

The next request from the client:

- `/api/auth/me` returns `orgs[]` without Loom (the membership is
  gone). The org switcher dropdown re-renders without it.
- If the active session was on Loom, the API responds with 403
  `not a member of the requested org`; the frontend's existing
  error handler should switch to the home org and reload.
- The `user_notice` `onboarding_graduated` renders as a one-time
  banner on the dashboard: "You've graduated! Now set up your own
  namespaces and start streaming logs. Everything's in the
  [manual]."

There is no restart. The tour is a one-shot — graduation revokes
membership and the welcome modal never reappears. Users who want
a refresher have the manual.

## API additions

```
POST /api/onboarding/advance     {step: "<expected>"}
  → 200 if user.onboarding_step == expected, advances to next.
    Idempotent on race (two clients advance at once).

POST /api/onboarding/dismiss
  → 200, sets dismissed=true. Doesn't revoke membership;
    user keeps Loom in their org list, just no overlays.
```

The OnboardingHooks server-side path doesn't go through these
endpoints — it advances directly via store. The endpoints are
only for client-driven advances (clicking a sidebar item, hitting
Next) and the dismiss escape hatch.

## Build order (5 PRs)

### PR 1 — `demo-source` customer + producers + seed (ops + code)

Without this every later stage is hollow. Pre-req for
*everything* user-facing.

- Pick a slug (`loom`) and create the customer row on prod.
- Stand up an EC2 box running the existing `docker-compose.demo.yml`
  producer containers, pointed at prod ingest.
- Extend `cmd/weft-demo-seed` to populate cron/, bin/, dashboards,
  and notify.conf for `loom`. These have to be live-tickably real
  enough that "real cron history" actually works after a week of
  uptime.
- Eval: log into Loom directly, browse for ten minutes, decide
  whether anything visible is hollow.

Estimated: 2 person-days (mostly Terraform + a small extension to
weft-demo-seed). This is the single biggest force multiplier in the
plan.

### PR 2 — onboarding state + signup hook + revocation endpoint

- Migration: add `onboarding_step` + `onboarding_dismissed`
  columns to users.
- Signup hook: insert Loom TeamMember + NamespacePermission rows
  alongside the user's own org creation.
- The three `/api/onboarding/*` endpoints. Wire the existing
  `auth.SessionFromContext` for ownership checks.
- Migration backfill for existing users: skip — onboarding_step
  defaults to 'welcome', dismissed=false, but they have no Loom
  membership so the welcome modal shows once and any "switch to
  Loom" attempt cleanly 403s. The first thing they see is "Skip"
  and they're out. Acceptable for the small number of pre-launch
  users.

Estimated: 1 person-day.

### PR 3 — welcome modal + auto-switch (web)

- New `WelcomeModal.tsx`. Renders when step=welcome AND not
  dismissed. Two buttons: Start (calls `/api/onboarding/advance`,
  then `switchOrg('loom')`) and Skip (calls `/api/onboarding/dismiss`).
- Trivial CSS, lots of `data-testid`s for testing.

Estimated: half a person-day.

### PR 4 — panel tour (web)

- New `TourOverlay.tsx` that reads onboarding_step and renders
  anchored overlays for the dashboards/namespaces/downloads steps.
- Each overlay has a `[Next]` button that POSTs advance.
- Sidebar items get a `data-onboarding-highlight` attr while the
  matching step is active; CSS pulse animation.
- Auto-advance when the user navigates to the route via NavLink
  click — wire a route-change listener to call advance if the
  current path matches the current step's target.
- Step `downloads → shell-ls` advance happens via a final overlay
  with a "Drop into the shell" button.

Estimated: 1.5 person-days.

### PR 5 — instrumented shell + lesson hooks

The biggest unit. Self-contained and shippable in increments
(start with 3 lessons, add more as follow-ups).

- New `coordinator/onboarding/` package with the `Lesson` type and
  the 13-entry slice.
- New `OnboardingHooks` interface in shellcore, parallel to
  `VimHooks`. The shell session attaches hooks when conditions are
  met (active org=loom, step in shell range, not dismissed).
- Server-side advance via store; client polls `/api/auth/me` for
  step changes.
- React side: `TourOverlay` extends to render the shell-bar copy +
  the `data-onboarding-highlight` toggling on `shell-chrome-btn`s
  for steps that target a sidebar.

Initial ship: lessons `shell-ls`, `shell-cat`, `shell-grep-awk`,
`shell-alert`. Rest come as fast follow-ups so we can refine the
hint copy without re-shipping the framework each time.

Estimated: 4 person-days for the framework + 4 lessons; ~half a
day per additional lesson.

## Resolved decisions

1. **Loom Inc** is the org name. Thematically aligned with weft;
   reads better than `demo-source`.
2. **CLI / web parity.** Same lesson set across both. Lesson copy
   leads with the CLI form ("type `cat /log/loom/api.log`") and
   appends the web alternative ("or in the web UI, click the
   stream in the dashboard"). The shell hooks are universal — the
   only divergence is that the highlighted UI buttons and the
   `data-onboarding-highlight` pulse only render in the web. The
   CLI gets ANSI-only inline hints.
3. **No restart.** Tour is a one-shot per account. People who want
   a refresher have the manual.

## Pre-existing data-model holes worth fixing alongside

**`saved_queries` is user-keyed, not org-keyed.** Every other
shell-managed resource (`shell_functions`, `cron_watches`,
`tmp_files`) is `(customer_id, …)` and gets version history via
the same key. Saved queries break that pattern: a query the user
saves while scoped to Loom follows them back into their own org
after graduation, and we have no way to filter them by org for
graduation cleanup.

The right fix is a separate migration that adds `customer_id` to
`saved_queries` and updates the handlers. Until that lands, the
graduation step has two unappealing options:

- Delete *all* of the user's saved queries (loses any they typed
  intending to keep).
- Skip cleanup (a few onboarding-era queries leak into their own
  org's view).

Recommended: ship the saved_queries org-keying migration as a
prerequisite to PR 5 (the instrumented shell), so by the time the
shell tour can ask the user to save a query, the cleanup path is
clean. Estimated separately: half a person-day.

(The other shell virtual filesystems — tmp files, bin/, cron.d/,
notify.conf — are already org-keyed. This is an isolated gap.)
