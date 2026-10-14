# Audit plan

Audit of the user-facing docs (`docs/manual/*.md`, `docs/shell/*.md`; 30 files) for accuracy and style. Results: `SUMMARY.md`. This file is the plan and its current status.

## Standard: what counts as wrong

**Behavior claims** (what a command, flag, default, limit or error does) are verified black-box: run the documented command, compare actual output to the doc.

**Examples are illustrative.** A sample path, namespace, service, cron, script, alias, file or host does not have to exist; readers substitute their own. Short, readable names are preferred. An example is wrong only if:
1. its **syntax** is invalid, i.e. it would not parse or run even if every name in it existed (for example `/log/myco/api.log`, which has no kind segment, or `jq` without `-c` feeding a JSON-path tag); or
2. its stated **output or behavior** does not happen when the names are real; or
3. it depends on something the reader has no way to create from the doc's own steps (a setup the page never mentions).

Verify examples by **substituting real names** from the dev data and running the result, not by running the sample verbatim. "unknown source", "does not exist" and similar errors caused only by a made-up name are not findings.

Path facts (checked 2026-10-12): `/log/<ns>/<svc>/<kind>.log` (3 segments) and `/log/<ns>/<env>/<svc>/<kind>.log` (4 segments) are both valid forms; the shell names the 3-segment form canonical for env-less sources. A bare `<name>.log` means the namespace wildcard `/log/<name>.log`, so it does not mean "service `<name>`".

## Phases and status

| phase | what | status |
|---|---|---|
| 0 | ground rules, environment (dev stack, minted token, isolated `HOME`) | done |
| 1 | extract every behavioral claim into a per-doc ledger (1,636 claims) | done |
| 2 | black-box verification; verdicts + evidence per claim | done, **needs re-triage under the standard above (phase 5)** |
| 3 | style review sheets (`style/<doc>.md`) and manual-check list (`manual-checks.md`) | done |
| 4 | fix the docs, then regenerate | in progress: `alerting-strategies` and `tmp` edited (uncommitted); generated files not regenerated |
| 5 | re-triage verdicts under the illustrative-examples standard | **done 2026-10-12**: 32 rows downgraded to ✅, 4 reclassified doc-fault-syntax, 38 kept (each row carries a `[re-triaged ...]` note in its ledger) |

## Phase 4: fixing the docs

Order: most ❌ first. Done: `alerting-strategies`, `tmp` (doc-fault items only). Next: `query-logs`, `man-alert`, `metavars`, `man-less`, `weft-funcs`, `team`, `tokens`, then the rest by ❌ count.

Rules for each doc:
- Edit only the markdown source. Do not touch `frontend/src/generated/docs/*` or `shell/help_corpus_gen.go` (your in-flight generated files); check the render with `docgen` into a scratch directory and diff.
- Change only things where the doc is at fault. Anything that depends on a product decision is listed in the style sheet and left alone.
- Re-run every replacement form against live data before writing it.
- Log what was applied at the bottom of `style/<doc>.md`.
- After a batch, run `go generate` yourself and build the frontend before committing.

## Phase 5: re-triage worklist (complete; kept for reference)

Outcome: the made-up-name rows mostly dropped. Reclassified doc-fault-syntax: hb-03 (`/log/<app>.log` is the namespace wildcard), weftd-118 (cron example lacks `# schedule:` header, says watch), heroku-19/20 (step 1 mints `--env prod`, so the 3-segment `ls` needs the env segment). Still ❌ with real names: alerting-strategies-07 (filter runs before `tail`), scripts-45 (/bin script after a log read: exit 127), metavars-29 (mid-path globs), query-logs-08/09 (`cat *.log` / `tail *.log` in a service dir return empty, no error). man-less less-01/02/25 are ❌ only because `man`/`help`/`less` don't exist under `sh -c`; candidates for 🖥.

These ❌/⚠️ rows have evidence mentioning a missing source, nonexistent name or stale layout. For each: substitute a real name and re-run; keep, downgrade to ✅, or reclassify as doc-fault-syntax. Keyword match only, so some are real (bare `api.log`); expect many to drop.

- `help-builtins` (6): hb-03, hb-04, hb-24, hb-30, hb-37, hb-48
- `tmp` (6): tmp-03, tmp-08, tmp-15, tmp-18, tmp-38, tmp-45
- `weftd` (6): weftd-13, weftd-79, weftd-91, weftd-92, weftd-109, weftd-118
- `metavars` (5): metavars-05, metavars-28, metavars-29, metavars-31, metavars-34
- `query-logs` (5): query-logs-04, query-logs-08, query-logs-09, query-logs-39, query-logs-82
- `alerting-strategies` (4): alerting-strategies-06, alerting-strategies-07, alerting-strategies-11, alerting-strategies-12
- `heroku` (4): heroku-09, heroku-19, heroku-20, heroku-42
- `man-alert` (4): man-alert-29, man-alert-52, man-alert-56, man-alert-58
- `namespaces` (4): namespaces-14, namespaces-21, namespaces-22, namespaces-25
- `crons` (3): crons-06, crons-11, crons-23
- `render` (3): render-24, render-25, render-34
- `scripts` (3): scripts-11, scripts-42, scripts-45
- `sharing` (3): sharing-02, sharing-25, sharing-38
- `cloudwatch` (2): cw-08, cw-35
- `help-alerts` (2): help-alerts-30, help-alerts-49
- `man-chart` (2): man-chart-46, man-chart-49
- `tokens` (2): tok-18, tok-19
- `help-cron` (1): hcron-19
- `man-clipboard` (1): man-clipboard-34
- `man-dashboard` (1): man-dashboard-17
- `team` (1): team-38

Also recheck, because they rest on made-up names: sample crons that don't exist (`error-rate-hourly`, `pending-jobs`, `top-errors`), scripts (`/bin/check`, `/bin/error-rate`), files (`orders.csv`), aliases (`critical`), and the `/log/myco/...` examples in every doc.

## Decisions (made 2026-10-12) - STATUS: all code and doc work done 2026-10-12 except the items under 'Still open'

Product (code) changes:
- **Hard line count vs predicate pushdown.** A `head`/`tail -n N` stops later predicates from being pushed down (the filter runs on the N rows). Fix in the planner; supersedes the "limit-safe pushdown" carve-out and `TestPushdown_LimitSafePushdown`. Cron examples never use `-n` (crons are range-bound), and docs call out that `-n` can truncate results.
- **Rename watch to cron everywhere:** usage strings, errors, `watch:` YAML key, webhook payload key, `/watch/` route. No compatibility shim (no paying customers); update dogfood webhook consumers.
- **`/etc/notify.yaml`:** redact signing secrets on read (`cat` shows a placeholder; edits keep the stored secret unless replaced).
- **Fix in code:** awk `~` with a quoted string; `adduser --help` (print usage, create nothing); `/bin` script after a log read (compute exit 127); a token spans several namespaces (reject); `touch` resets the `/tmp` 48h clock and `cp /bin/x /tmp/x` follows the documented rule; `weft::is_absent` on a missing cron gets one defined behavior; Lua sandbox: remove `loadstring`, keep `os.date` and list it in the doc; `weft::` window functions accept `d` and `w` (keep the 14d cap documented); `ls /log/<ns>/<svc>` (3-segment) lists kinds merged across envs; `less` paging (in-process ModeLess in shellcore).
- **Ops:** rebuild the dev stack from HEAD to verify the fixes live; inspect why `https://w3ft.net/install.sh` returns HTML (read-only).

Docs-only:
- `owner`: the role value is `admin` (`store.RoleOwner = orgs.RoleAdmin`); docs say admin, "owner" only as a defined prose alias.
- `alert` as last/sole command only: document as-is.
- `mkdir` in `/tmp`: document flat, no directories.
- Plan limits: remove the numbers; point to the Account page.
- Composite `alert` recipes: rewrite around time ranges once the pushdown change lands.

Already done: the four bug fixes (`ace5ca6`) and the glob rule (`d0f3885`).

## Still open (2026-10-12, after the doc passes, rename, redaction and less)

- **Commits are local and unpushed** in weft and warp. weft HEAD includes the doc pass (`7d9b555`) and the rename (`563017d`).
- **Generated files not committed:** `frontend/src/generated/docs/*` and `shell/help_corpus_gen.go` were regenerated from the new docs, but your in-flight `cmd/docgen` change (man pages for web-only topics) is in the same output, so they were left in the working tree. Commit them together with that work; CI that checks generated-vs-source will flag the md edits until then.
- `https://w3ft.net/install.sh` returning HTML: not investigated yet.
- Web/wasm shell pager: same shellcore as the CLI but never run in a browser.
- Product items the doc agents left: wrong-kind dashboard tile keys accepted silently; `alert` only as the first statement of a cron body; awk uninitialised var `==0` is false; `alert absent` fires when the upstream query fails; `weft::previous` has no bash form; `help-index` tier-limit ownership; weftd `schema:`/`prefix_fields`/`json_field` doc sections look stale; dev dashboards and crons carry audit residue (namespaces `acme,globex`, `acmee`, `audit-metavars-ns`, revoked tokens 19-25, an invite row named `alice`).
- Webhook header redaction covers header VALUES only; a secret embedded in a webhook URL is still shown.

## Environment and harness notes

- Dev stack on OrbStack (`docker start` brought it back after a restart); API `http://localhost:8080`; seeded users alice/bob/carol (acme), dana/eli/frida (globex), kai/priya (loom).
- Tokens are minted with `mint.py` (HS256, `sub` = user id, secret `dev-secret-change-in-production`).
- `weft sh -c` runs pipelines with zero write permission by design, so persistent writes (`/bin`, `/cron.d`, `/etc`) are not tested; `help`, `man`, `vi`, `dashboard` and the pager need an interactive terminal. Those claims are in `manual-checks.md`.
- Use a private `HOME` per run so `~/.weft/proposals` is never shared. Never run `weft propose apply/clear`, `adduser`/`deluser`, or the host `mcp__weft__*` tools in an audit run.
