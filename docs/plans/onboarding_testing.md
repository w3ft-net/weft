# Plan: Onboarding tour — testing and known bugs

**Status (2026-10-10):** A full manual pass of the web tour (comp account → login link → welcome modal → 3 panel steps → 26 shell lessons → graduation) runs clean on the dev stack with no workarounds. This doc records how to repeat that pass, what is verified, and the bugs and gaps that remain, in priority order.

Related: `docs/completed/onboarding.md` (the original design), `coordinator/onboarding/lessons.go` (the lessons), `api/onboarding.go` (state machine, graduation).

## How the tour advances (read this before debugging a "stuck" step)

Progress is `users.onboarding_step`, advanced by compare-and-swap, so duplicate reports are harmless. There are two independent reporters:

1. **Server exec path** — `handleShellExec` calls `maybeAdvanceShellLesson` when a pipeline's stream ends (or the client disconnects). It only sees the pipeline text the client chose to send, so it cannot see client-side pipe stages (`| chart …`) or inline builtins (`history`). Used by the CLI and still active for the web shell.
2. **Browser completion report** — the shell core publishes `CommandCompleted{line, exitCode}` for every line; `cmd/wasm` forwards it as `commandCompleted(line, exitCode)`; `network.ts` `reportCommandCompleted` posts `{pipeline, exit_code}` to `POST /api/shell/builtin` and then fires a `tour-advance` event so the card refreshes.

Rules enforced server-side (`maybeAdvanceShellLesson`, `onboarding.AdvancesOnExit`): a lesson advances on exit 0; exit 130 (Ctrl+C) advances only a lesson with `CompletesOnInterrupt` (today: `shell-tail-f`); any other non-zero exit never advances. Lesson matching is exact on `Action` unless `ActionPattern` is set (`shell-trace`).

Every lesson step must start with `shell-` or `TourOverlay` unmounts (pinned by `TestEveryLessonStepHasShellPrefix`).

## Manual end-to-end pass

Prereqs: dev stack up (`dev/demo/up.sh`; Loom now runs the real Go `cmd/loom-producer`, so trace ids exist), `cd frontend && npx vite --port 5173`, the wasm built (`GOOS=js GOARCH=wasm go build -o frontend/public/shell.wasm ./cmd/wasm/`), and a superadmin to call the admin API.

1. Create the account: `POST /api/admin/customers {email, org_name, tier, reason}`; the response carries `login_url`. (Tokens are SHA-256 hashed in `magic_links`, so the raw token can only come from that response, the email, or the API log.)
2. Open `login_url` in a clean profile/localStorage. Expect exactly one `POST /api/auth/verify` (200) and the welcome modal.
3. "start the tour", then click the highlighted sidebar items: downloads → namespaces → dashboards. Expect cards 1/3, 2/3, 3/3, then the shell card.
4. In the shell, press Enter on each prefilled command. After each, the next command should be prefilled with no action from you. Copy a real trace id for `shell-trace` (do not use a placeholder).
5. `shell-bin-lua`: edit, `:w` (expect the `binmgr` permission message), `:q!`. The step must not advance until `:q!`.
6. `cat /etc/usage` graduates: expect `/dashboards` in the user's own org, the green banner, user still logged in; DB: own org `active`, `loom` `removed`, `onboarding_step = done`, zero `/api/auth/me` 403s.

Pitfalls when driving the browser (these cost hours once):
- **The window must be visible.** In a background window `document.visibilityState` is `hidden`: xterm.js stops repainting and ignores input, and screenshots/DOM reads go stale.
- Clear the input line (`ctrl+u`) before typing over a prefill (see OB-6).
- Read terminal output from `.xterm-rows` (`innerText`), not screenshots, which crop the bottom of the page.
- The shell's dirstack: `cd` only sticks if the dirstack courier is wired (client sends `dirstack`, applies the server's `cwd` event).

## Lesson status (web, dev stack, 2026-10-10)

All 26 lessons verified live in xterm through graduation, in order: `ls /log`, `tree /log/loom`, `range 1h..`, `tail`, `tail -f` (+Ctrl+C), `cd`, grep/awk, trace-find, `trace <real id>`, alert, `cat /etc/notify.yaml`, `ls -l /cron.d/`, `cat /cron.d/error-rate`, `ls /bin/`, `wit log /bin/scan -p`, `vim /bin/severity`, `cat /etc/passwd`, `clipboard /bin/scan`, `weft::avg error-rate 1h`, `history`, `echo hello > /tmp/note`, `chart error-rate`, chart pipe stage, `cat /etc/dashboards/prod.yaml`, `dashboard prod`, `cat /etc/usage`.

Not verified: email delivery (OB-11), prod data (OB-10). The CLI tour is out of scope.

## Fixed during this pass (regression coverage)

| Bug | Fix | Test |
|---|---|---|
| Graduation removed the user's *own* org membership (locked out, `/login`) | `graduateOnboarding` uses org-scoped `Orgs.RemoveMember` | `api/onboarding_graduation_test.go` |
| `history` and the chart-pipe lesson never advanced | browser reports completed lines | `shell_bridge.test.ts` (real wasm), `network_routing.test.ts`, `api/shell_builtin_test.go` |
| Failed commands advanced lessons | server gates on `exit_code` | `api/shell_builtin_test.go`, `TestAdvancesOnExit` |
| Ctrl+C on `tail -f` (exit 130) left a stale card | `CompletesOnInterrupt` | `TestAdvancesOnExit`, `api/shell_builtin_test.go` |
| `chart` step name made the overlay vanish | renamed `shell-chart` | `TestEveryLessonStepHasShellPrefix` |
| Vim lesson advanced on open | warp completes `vim <path>` on exit | warp `TestEvents_VimCompletesOnExitNotOnOpen` |
| `cd`/`ls`/`tree` on `/log/<ns>/…` rejected (b5c68b6 regression) | planner filesystem-command allowlist | `planner/fs_commands_test.go`, `coordinator/query_rewrite_test.go` |
| Web shell `cd` never stuck | dirstack courier (warp exports + `network.ts`) | `network_routing.test.ts`, `shell_bridge.test.ts` |
| `tail -f` error events dropped (`message` vs `text`) | `network.ts` reads both | `network_routing.test.ts` |
| Verify fired twice (200 then 401) | one in-flight verify per token | `verify_once.test.tsx` |
| "drop into the shell" did not navigate | `location.assign('/shell')` | `tour_overlay_nav.test.tsx` |
| Dev Loom had no trace ids; trace lesson showed 1 line from 1 service | dev runs the Go producer; lesson greps `' checkout '` | `lesson_trace_test.go`, `plan_test.go` |

## Known bugs and plan (priority order)

**Coded, not yet re-verified in the browser: OB-2, OB-5, OB-3, OB-4, OB-6, OB-7, OB-8, OB-9** (each has a test; the next manual pass should confirm them live). OB-4 caveat: the `[x]` button already closed the modal in a component test, so the observed "does nothing" was not reproduced; what was missing was Esc (now a capture-phase handler, so xterm behind the backdrop no longer sees it), refocus of the terminal on close, and chart/trace closing each other. The `shell-trace` lesson now completes on the modal closing, not on `trace` finishing: `ShellTraceModal` reports `event: modal_closed` from unmount (only for a trace that loaded), its own row fetch carries `modal: true` so the exec path ignores it, and `onboarding.CompletesOn` holds the rules. The CLI, which has no modal, still advances on exec. OB-6/OB-8 live in warp (`pkg/shellcore`, uncommitted there) and need the warp release + pin bump below.

### OB-2 — The designed (sidebar) path ends without a way into the shell
Clicking the highlighted sidebar items through the last panel step lands on the dashboards page with the shell card ("Hit enter…") but no shell and no button; only the "next"/"drop into the shell" fallback navigates. A user following the highlights must guess to open the shell.
Plan: when the step becomes the first `shell-*` lesson and the route isn't `/shell`, either navigate there automatically or show a "drop into the shell" button on the shell card. Test: extend `tour_overlay_nav.test.tsx` — advancing from the last panel via the sidebar link ends on `/shell` (or the card renders the button).

### OB-3 — Absolute paths in tour commands after `cd`
Lesson 6 (`shell-cd`) teaches that after `cd /log/loom/prod/api` you can drop the long prefix ("`tail main.log` instead of the full path"). The very next lessons then prefill absolute paths, which undercuts the lesson and looks odd. Affected lessons (all after `shell-cd`):

| Lesson | Current Action | Proposed Action |
|---|---|---|
| `shell-grep-awk` | `tail /log/loom/prod/api/main.log \| grep ERROR \| awk '{print HOST, $1, $5}'` | `tail main.log \| grep ERROR \| awk '{print HOST, $1, $5}'` |
| `shell-trace-find` | `tail /log/loom/prod/api/main.log \| grep ' checkout ' \| awk '{print TRACE_ID}'` | `tail main.log \| grep ' checkout ' \| awk '{print TRACE_ID}'` |
| `shell-alert` | `tail /log/loom/prod/api/main.log \| grep ERROR \| wc -l \| alert above 0` | `tail main.log \| grep ERROR \| wc -l \| alert above 0` |
| `shell-chart-pipe` | `tail /log/loom/prod/api/main.log \| grep ERROR \| chart -x 1 --bucket 5m` | `cat main.log \| grep ERROR \| chart -x 1 --bucket 5m` (see OB-5) |

Leave `/etc`, `/cron.d`, `/bin`, `/tmp` lessons absolute — they are different mounts and read naturally that way. Leave the preseeded cron/bin scripts absolute (they run outside any cwd).
Dependencies: relative paths only work if the cwd is shipped with the request (web courier now does; the CLI already does). Verified manually that `tail main.log` resolves after `cd`.
Risks and plan:
- A user who `cd`s elsewhere before these lessons gets an error on the verbatim command. Mitigate in copy ("run this from /log/loom/prod/api") and/or have the card detect a non-matching cwd and show the `cd` line first.
- Update the prompts that mention full paths, and `lessons_test.go`'s expected table.
- Contract test in `coordinator/onboarding`: after `shell-cd` in the sequence, no `Action` contains an absolute `/log/` path. The `lesson_trace_test.go` pattern extraction keeps working (it reads the grep argument only).
- Re-run the pass with these commands in xterm to confirm each still matches exactly after the prefill.

### OB-4 — Trace panel never closes and overlaps other UI
After `trace <id>` the result panel stays on screen for the rest of the session: Esc and its `[x]` do nothing, and it draws over vim and the chart panes (it hides the editor's lines and the chart).
Plan: find the panel's owner (`traceOpen` callback / React trace view), make Esc and `[x]` dismiss it, and close it when another pane/overlay opens or a new command runs. Test: component test that Esc and `[x]` unmount it; manual pass step 4.

### OB-5 — Chart-pipe pane shows raw rows instead of a chart (coded, not browser-verified)
Two causes. (1) The web shell's pipe chart was a stub: `chartRequestPipe` dropped `-x/-y/--bucket/-a/--tag` and `ShellChartModal` printed the upstream rows in a `<pre>`. Now `chart.PlotFromStream` (shared with the CLI renderer) reduces the rows in Go, `cmd/wasm` ships the plot as JSON, and the modal draws it (Sparkline for line/sparkline, bars for bar); a single bucket says so instead of drawing nothing. (2) The lesson charted `tail … | grep ERROR`: ~10 rows at ~8% errors is 0–2 lines. It is now `cat main.log | grep ERROR | chart -x 1 --bucket 5m`, which covers the whole range (the tour has set `range 1h..` by then → ~12 buckets).
Tests: `chart/plot_test.go`, `shell/chart_request_test.go`, `shell_chart_modal.test.tsx`, a real-wasm case in `shell_bridge.test.ts`, `lessons_test.go`.
Verify live: the lesson draws a line with several points on dev data (check the bucket size against dev error rates).

### OB-6 — Typing over a prefilled command concatenates
The tour prefills the next command into the input. Typing instead of pressing Enter appends to it (`cat /etc/notify.yamlcat /etc/notify.yaml`), then fails.
Plan: decide intended behavior — first keystroke replaces an untouched prefill (like a placeholder), or keep append and rely on the card's paste/clear affordance. Implement in the shell core/embedder; test in `bus_wiring_test.go`-style input tests plus a bridge test.

### OB-7 — Lesson 2 copy does not match its command
`shell-ls-loom` says "Now `ls` again to see what's in /log/loom" but the Action is `tree /log/loom`.
Plan: fix the Prompt (or the Action) in `lessons.go`; trivial, covered by the lessons table test.

### OB-8 — `[tail stopped]` prints twice
Cosmetic. Suspect both the follow-stop path and the core's cancel path write it.
Plan: find the two writers (`network.ts`/`cancelQuery` vs the core's Ctrl+C handling), emit once; assert on terminal output in a bridge test.

### OB-9 — `ls` on a service directory fails
`ls /log/loom/prod/api` and a bare `ls` after `cd /log/loom/prod/api` report "file does not exist"; the log mount lists services as flat `api/main.log` entries under `/log/loom/prod`, not as directories, yet `cd` into one succeeds. Predates the planner fix.
Plan: make the log mount (shellfs `LogMount`) treat `<ns>/<env>/<svc>` as a directory (list `main.log`, `metrics.log`), consistent with `cd`. Test in `shellfs`.

### OB-10 — Prod verification
Dev now matches the prod producer (same binary and `streams.yaml`, `trace_id_field: "3"`), but prod itself was not exercised. Check on prod (read-only): `awk '{print TRACE_ID}'` returns ids, `trace <id>` returns a 3-service chain for a `checkout` id, and which build is deployed — `b5c68b6` (the planner regression) has been on `origin/main` since 2026-09-19 while `ls /log/<ns>` still worked on prod through the MCP tools, so confirm whether prod is on a build with the regression before treating it as an incident. Verify the deploy before diagnosing.

### OB-11 — Email delivery untested
Dev logs the magic link; SMTP was never exercised. Plan: send a real magic-link email through the configured SMTP from a non-prod environment, confirm link host/base URL and the 15-minute expiry text.

### OB-12 — No automated end-to-end coverage
All of the above was found by hand. Plan: a scripted pass (Playwright driving the real xterm, headless so window visibility is not a factor) that creates a comp account, runs the tour verbatim (using a real trace id read from the terminal), and asserts the DB end state. Reuse the existing `data-testid`s (`welcome-modal-start-button`, `tour-overlay-*`, `sidebar-*-link`). Run it against the dev stack in CI after `dev/demo/up.sh`.

## Ship blockers (not bugs)
- **warp release + pin bump.** The dirstack exports, `history` test and vim-completion fix live in warp (`bdd09a8` and the follow-up). weft's `go.mod` pins `v0.2.42`; CI builds of the web shell need a warp release and a weft pin bump first. Until then this works only through the local `go.work`.
- Deploy note: the new wasm and the new API can ship in either order (an old server ignores `exit_code`; an old wasm just doesn't report). The one data-affecting change is the `chart` → `shell-chart` rename: a user currently *on* step `chart` would be stranded. None exist outside the dogfood account today; check before deploying.
