# Docs audit summary

Audit of the user-facing docs (`docs/manual/*.md`, `docs/shell/*.md`; 30 files) for accuracy and style, run on 2026-10-12 against the local dev stack. Claims were extracted from each doc, then verified black-box with the `weft` CLI as a seeded user. Nothing in the docs, source or generated files was changed.

## How to use this

1. **Style review (you):** open `style/<doc>.md`. Each has a 13-point checklist pre-filled with my read, the fixes the audit already established, and the decisions that are yours. Start with the docs at the top of the table below (most ❌).
2. **Manual checks (you):** `manual-checks.md` lists the 201 terminal/browser checks and 407 write/account checks the audit could not do, with the exact step for each.
3. **Plan and status:** `PLAN.md` has the verification standard, phase status, the fix order, and the re-triage worklist.
4. **Evidence:** each `<doc>.md` is the full ledger: one row per claim with verdict and the command and output that justify it, plus a Findings section.

Verdicts: ✅ verified · ❌ wrong (doc and behavior differ) · ⚠️ partly true or ambiguous · 🔒 needs a write, account or human · 🖥 needs a terminal or browser · ∅ unverifiable black-box.

## Results

| doc | claims | ✅ | ❌ | ⚠️ | 🔒 | 🖥 | ∅ | style: change / your call | files |
|---|---|---|---|---|---|---|---|---|---|
| `manual/alerting-strategies` | 73 | 20 | 10 | 20 | 14 | 6 | 3 | 6 / 4 | [ledger](alerting-strategies.md) · [sheet](style/alerting-strategies.md) · [manual](manual-checks.md#alerting-strategies) |
| `manual/tmp` | 55 | 14 | 9 | 16 | 11 | 2 | 3 | 6 / 4 | [ledger](tmp.md) · [sheet](style/tmp.md) · [manual](manual-checks.md#tmp) |
| `manual/query-logs` | 95 | 42 | 6 | 26 | 9 | 1 | 11 | 8 / 2 | [ledger](query-logs.md) · [sheet](style/query-logs.md) · [manual](manual-checks.md#query-logs) |
| `manual/metavars` | 59 | 13 | 5 | 22 | 8 | 1 | 10 | 8 / 2 | [ledger](metavars.md) · [sheet](style/metavars.md) · [manual](manual-checks.md#metavars) |
| `shell/man-alert` | 61 | 23 | 5 | 21 | 9 | 3 | 0 | 6 / 3 | [ledger](man-alert.md) · [sheet](style/man-alert.md) · [manual](manual-checks.md#man-alert) |
| `manual/team` | 48 | 10 | 4 | 5 | 25 | 4 | 0 | 4 / 2 | [ledger](team.md) · [sheet](style/team.md) · [manual](manual-checks.md#team) |
| `manual/tokens` | 27 | 8 | 4 | 4 | 6 | 4 | 1 | 4 / 4 | [ledger](tokens.md) · [sheet](style/tokens.md) · [manual](manual-checks.md#tokens) |
| `shell/man-less` | 25 | 0 | 4 | 2 | 0 | 17 | 2 | 4 / 3 | [ledger](man-less.md) · [sheet](style/man-less.md) · [manual](manual-checks.md#man-less) |
| `manual/weftd` | 123 | 11 | 3 | 20 | 80 | 2 | 7 | 8 / 2 | [ledger](weftd.md) · [sheet](style/weftd.md) · [manual](manual-checks.md#weftd) |
| `shell/help-builtins` | 84 | 34 | 3 | 19 | 14 | 13 | 1 | 6 / 2 | [ledger](help-builtins.md) · [sheet](style/help-builtins.md) · [manual](manual-checks.md#help-builtins) |
| `manual/weft-funcs` | 60 | 34 | 3 | 12 | 0 | 2 | 9 | 7 / 1 | [ledger](weft-funcs.md) · [sheet](style/weft-funcs.md) · [manual](manual-checks.md#weft-funcs) |
| `manual/namespaces` | 45 | 13 | 3 | 9 | 18 | 0 | 2 | 4 / 2 | [ledger](namespaces.md) · [sheet](style/namespaces.md) · [manual](manual-checks.md#namespaces) |
| `shell/help-alerts` | 52 | 13 | 2 | 16 | 13 | 4 | 4 | 7 / 2 | [ledger](help-alerts.md) · [sheet](style/help-alerts.md) · [manual](manual-checks.md#help-alerts) |
| `manual/scripts` | 53 | 18 | 2 | 14 | 11 | 4 | 4 | 8 / 2 | [ledger](scripts.md) · [sheet](style/scripts.md) · [manual](manual-checks.md#scripts) |
| `manual/heroku` | 56 | 3 | 2 | 13 | 29 | 4 | 5 | 4 / 2 | [ledger](heroku.md) · [sheet](style/heroku.md) · [manual](manual-checks.md#heroku) |
| `shell/man-clipboard` | 46 | 15 | 2 | 9 | 1 | 18 | 1 | 5 / 2 | [ledger](man-clipboard.md) · [sheet](style/man-clipboard.md) · [manual](manual-checks.md#man-clipboard) |
| `shell/help-cron` | 37 | 14 | 2 | 8 | 6 | 7 | 0 | 4 / 3 | [ledger](help-cron.md) · [sheet](style/help-cron.md) · [manual](manual-checks.md#help-cron) |
| `shell/man-passwd` | 49 | 13 | 2 | 5 | 26 | 1 | 2 | 3 / 3 | [ledger](man-passwd.md) · [sheet](style/man-passwd.md) · [manual](manual-checks.md#man-passwd) |
| `manual/dashboards` | 65 | 26 | 1 | 14 | 0 | 23 | 1 | 5 / 2 | [ledger](dashboards.md) · [sheet](style/dashboards.md) · [manual](manual-checks.md#dashboards) |
| `manual/crons` | 36 | 12 | 1 | 11 | 5 | 7 | 0 | 4 / 3 | [ledger](crons.md) · [sheet](style/crons.md) · [manual](manual-checks.md#crons) |
| `manual/render` | 48 | 2 | 1 | 11 | 22 | 2 | 10 | 5 / 2 | [ledger](render.md) · [sheet](style/render.md) · [manual](manual-checks.md#render) |
| `manual/git` | 129 | 62 | 0 | 17 | 41 | 9 | 0 | 6 / 1 | [ledger](git.md) · [sheet](style/git.md) · [manual](manual-checks.md#git) |
| `manual/cloudwatch` | 46 | 3 | 0 | 12 | 22 | 7 | 2 | 3 / 3 | [ledger](cloudwatch.md) · [sheet](style/cloudwatch.md) · [manual](manual-checks.md#cloudwatch) |
| `shell/man-chart` | 53 | 26 | 0 | 12 | 0 | 13 | 2 | 5 / 3 | [ledger](man-chart.md) · [sheet](style/man-chart.md) · [manual](manual-checks.md#man-chart) |
| `manual/sharing` | 39 | 11 | 0 | 12 | 4 | 9 | 3 | 5 / 1 | [ledger](sharing.md) · [sheet](style/sharing.md) · [manual](manual-checks.md#sharing) |
| `manual/notify` | 33 | 10 | 0 | 9 | 10 | 3 | 1 | 3 / 2 | [ledger](notify.md) · [sheet](style/notify.md) · [manual](manual-checks.md#notify) |
| `shell/man-dashboard` | 56 | 3 | 0 | 8 | 22 | 22 | 1 | 3 / 4 | [ledger](man-dashboard.md) · [sheet](style/man-dashboard.md) · [manual](manual-checks.md#man-dashboard) |
| `shell/help-index` | 21 | 13 | 0 | 6 | 0 | 2 | 0 | 2 / 3 | [ledger](help-index.md) · [sheet](style/help-index.md) · [manual](manual-checks.md#help-index) |
| `shell/man-range` | 48 | 38 | 0 | 5 | 0 | 5 | 0 | 0 / 3 | [ledger](man-range.md) · [sheet](style/man-range.md) · [manual](manual-checks.md#man-range) |
| `manual/schema-setup` | 14 | 3 | 0 | 4 | 1 | 6 | 0 | 2 / 2 | [ledger](schema-setup.md) · [sheet](style/schema-setup.md) · [manual](manual-checks.md#schema-setup) |
| **total** | **1636** | **507** | **74** | **362** | **407** | **201** | **85** | | |

## Cross-doc findings

### Probably product bugs (items 13-16 added by the phase 5 re-triage; the doc describes sensible behavior the code doesn't deliver)
1. **A filter runs before `tail`/`head`.** `tail -n 500 f | awk 'LEVEL=="ERROR"' | wc -l` returns 500, because the filter is applied first. Alert recipes in `crons`, `alerting-strategies`, `man-alert` and `metavars` misfire. Most serious.
2. **`less` doesn't page.** It prints raw, and `| less | wc -l` returns 0 (output swallowed). Non-final stages fail with exit 127.
3. **`touch` doesn't reset the `/tmp` 48h clock**, and `cp /bin/scan /tmp/x` isn't restricted as `tmp.md` says.
4. **A token can span several namespaces** (`--namespace acme,globex`), though `tokens.md` says one.
5. **Quoted-string `~` in awk never matches** (`SERVICE ~ "api"` returns 0; `/regex/` works).
6. **Directory globs fail** (`/log/acme/*/api/main.log`), filename globs work.
7. **`adduser --help` creates an invite for a user named `--help`.**
8. **The Lua sandbox exposes `os.date` and `loadstring`**, against the doc.
9. **`alert` only works as the last or sole command**, and drops stdin and the rest of the script when it fires.
10. **`weft::is_absent` on a missing cron** fires in one test (`weft-funcs`) and exits 1 in another (`weftd`).
11. **`weft` scripts as non-final pipeline stages** fail with `bash: <name>: command not found` (exit 127) on the dev compute worker; check on prod.
13. **FIXED in working tree (not deployed):** plain `ls FILE` failed for every file, and `ls /etc | wc -l`, `cat /etc/passwd | wc -l` and `ls /etc /bin` ignored everything after the first path. Cause: the one-shot shell's client-side `ls`/`cat` shortcut (`shell/shell.go` `execFSBuiltin`) read the first word only; removed, so the lines ship to the server. **Left alone:** `ls /log/<ns>/<svc>` (3-segment) still fails because the 3-segment form is a read shortcut, not a directory in the FS; its read spans every env by design.
14. **FIXED in working tree (not deployed):** `head -n 3 F | tail -n 1` returned the newest line. A head/tail after a frozen LIMIT rewrote the SQL and also ran in compute; it now stays in compute (`planner/planner.go`).
15. **FIXED in working tree (not deployed):** a glob in a source command that wasn't the line's first command (`cd X && tail *.log`, `;` chains, /bin bodies) was never expanded and silently matched `kind = '*'`. Expansion moved from `preflight` into `runQuery`; the planner now refuses an unexpanded glob.
16. **FIXED in working tree (not deployed):** `tail -f ... > /tmp/x` now fails with a clear error instead of writing a truncated file, so `tmp.md` is true as written.
12. **`https://w3ft.net/install.sh` returned HTML** (the web app), not a script. Check the deployment.

### Doc-stale patterns
- **Bare log names** (`api.log`, `myapp.log`) are the real path defect: a bare `<name>.log` means the namespace wildcard, or resolves to nothing, so those examples don't do what the doc says. **Correction:** earlier ledger findings calling the 3-segment form `/log/<ns>/<svc>/<kind>.log` "stale" came from env-bound dev data. That form is valid syntax (the shell's own error names it canonical for env-less tokens), so treat those findings as unconfirmed unless the doc's own steps create an env-bound token first (as `heroku.md` does).
- **"watch" instead of cron**, in prose, in usage strings and errors, the `watch:` YAML key, the webhook payload key and the `/watch/` route. The code-side names need a decision before the prose can be fixed.
- **Durations:** `1d`, `1w` and `14d` are rejected by `weft::` functions (h/m/s only); `range` accepts `m`, `h`, `d` only.
- **Examples reference things that don't exist** (`error-rate`, `/bin/check`, `pending-jobs`), or break without `jq -c`.
- **Terminology drift:** `owner` vs `admin`, an undocumented `nsmgr` group, `issued:` vs `Token created:`, `reissue` with no command.
- **Customer docs cite repo paths** (`docs/manual/notify.md`).

## Decisions that are yours
- Which of the 12 product-bug candidates to fix versus document as-is.
- Whether to rename `watch` in code (usage strings, YAML key, webhook payload key, route) or keep it and document it.
- `owner` vs `admin`: the dev seed has no owner, so the doc's owner examples are unobserved.
- Whether `/etc/notify.yaml` should hold plaintext webhook signing secrets (the doc says credentials live with the account).

## Caveats on the audit itself
- **Dev stack only.** Some findings may be dev-seed artifacts: no owner role, no env-less tokens, no `heartbeats.log`, metrics every 10s instead of 30s.
- **`weft sh -c` limits.** `help`, `man`, `vi`, `dashboard` and the pager are not available; `sh -c` runs with zero write permission by design (writes to `/bin`, `/cron.d`, `/etc` are denied by `coordinator/coordinator.go:1331`); `;` doesn't split statements. Those claims are 🖥 or 🔒.
- **Writes were skipped on purpose**, so anything needing a persistent write is 🔒.
- **A few verdicts rest on static reads**, not runs, and say so in the evidence cell (for example `heroku-09`, `render-12`, and the generated-help checks).
- **One finding retracted:** `help-index` hidx-05 (`help alerts` lacking `notify.yaml`/"dispatching"); the re-check found both.
- **Residue in the dev stack:** revoked tokens 16–20, namespace `audit-metavars-ns`, about ten short-link rows, a stray `alice … invited` roster row, and the globex crons and script (7 crons, 1 script) applied by an earlier mishap. Proposals 0001–0008 in `~/.weft/proposals/current` were consumed in that mishap; 0009 and 0011 remain.
