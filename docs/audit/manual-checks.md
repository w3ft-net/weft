# Manual checks

Everything the audit could not verify from a non-interactive, read-only `weft sh -c` against the dev stack.
Steps are copied from each ledger's evidence cell; the ledger row id points back to the full evidence.

- **Terminal or browser** (🖥): needs an interactive `weft sh`, a TTY, or a signed-in browser.
- **Write, account or human** (🔒): needs a persistent write (blocked here by design), a third-party account, or a human decision.

**Totals:** 201 terminal/browser checks, 407 write/account checks across 30 docs.

| doc | terminal/browser | write/account |
|---|---|---|
| [alerting-strategies](#alerting-strategies) | 6 | 14 |
| [cloudwatch](#cloudwatch) | 7 | 22 |
| [crons](#crons) | 7 | 5 |
| [dashboards](#dashboards) | 23 | 0 |
| [git](#git) | 9 | 41 |
| [help-alerts](#help-alerts) | 4 | 13 |
| [help-builtins](#help-builtins) | 13 | 14 |
| [help-cron](#help-cron) | 7 | 6 |
| [help-index](#help-index) | 2 | 0 |
| [heroku](#heroku) | 4 | 29 |
| [man-alert](#man-alert) | 3 | 9 |
| [man-chart](#man-chart) | 13 | 0 |
| [man-clipboard](#man-clipboard) | 18 | 1 |
| [man-dashboard](#man-dashboard) | 22 | 22 |
| [man-less](#man-less) | 17 | 0 |
| [man-passwd](#man-passwd) | 1 | 26 |
| [man-range](#man-range) | 5 | 0 |
| [metavars](#metavars) | 1 | 8 |
| [namespaces](#namespaces) | 0 | 18 |
| [notify](#notify) | 3 | 10 |
| [query-logs](#query-logs) | 1 | 9 |
| [render](#render) | 2 | 22 |
| [schema-setup](#schema-setup) | 6 | 1 |
| [scripts](#scripts) | 4 | 11 |
| [sharing](#sharing) | 9 | 4 |
| [team](#team) | 4 | 25 |
| [tmp](#tmp) | 2 | 11 |
| [tokens](#tokens) | 4 | 6 |
| [weft-funcs](#weft-funcs) | 2 | 0 |
| [weftd](#weftd) | 2 | 80 |

## alerting-strategies

Source: `docs/audit/alerting-strategies.md`

### Terminal or browser

- [ ] **alerting-strategies-01** (doc line 10-15) Page catalogues alert shapes: single-cron scalar, multi-row per-tag, composite
  - Open Manual > "Alerting strategies"; confirm category alert-visualize / order 53. Static: front matter has category alert-visualize, order 53; generated ts has section "alerting-strategies".
- [ ] **alerting-strategies-02** (doc line 13) `alert` builtin grammar lives in `man alert`
  - `man` not available under sh -c. Static: ManAlert in shell/help_corpus_gen.go exists and documents grammar. Type `man alert` in interactive weft sh.
- [ ] **alerting-strategies-03** (doc line 14) `weft::*` family documented in "Weft functions" doc
  - `help weft` unavailable under sh -c; check Manual page "Weft functions" in browser. docs/manual/weft-funcs.md exists.
- [ ] **alerting-strategies-70** (doc line 240) "Weft functions" doc covers value funcs, predicates, stdin comparators
  - Open Manual > Weft functions. Static: docs/manual/weft-funcs.md documents is_gt (line 130) and aggregates.
- [ ] **alerting-strategies-71** (doc line 242) "Crons" doc covers scheduled pipelines and incident lifecycle
  - Open Manual > Crons. Static: docs/manual/crons.md exists.
- [ ] **alerting-strategies-72** (doc line 243) `man alert` full grammar
  - `man alert` in interactive shell. Static: ManAlert present in corpus.

### Write, account or human

- [ ] **alerting-strategies-10** (doc line 38-39) Each row opens and closes incidents independently against its tag
  - Human: create cron, wait 2 ticks, `weft incidents`.
- [ ] **alerting-strategies-16** (doc line 49-51) One PagerDuty incident per affected host, not one giant incident
  - PagerDuty account needed. In-product: per-tag messages seen (row 9); `weft incidents` after a real cron run.
- [ ] **alerting-strategies-30** (doc line 99) Lua surface mirrors bash predicates one-for-one
  - Human: write /cron.d lua script exercising weft.is_above etc. Bash mirrors exist: is_above, is_below, is_absent, has_changed, is_rising, is_outlier, is_seasonal run.
- [ ] **alerting-strategies-31** (doc line 100-101) Lua composition: emit 1 or 0 and pipe to `alert above 0`
  - Needs Lua cron (persistent write). Step: create Lua cron that print(1) piped to `alert above 0`.
- [ ] **alerting-strategies-32** (doc line 103-112) Lua example cron with `-- schedule: every 1m` header comment, `weft.is_above("error-rate", 50)`, `print(1)`
  - Needs creating a Lua cron. Step: create /cron.d/deploy-danger with `-- schedule: every 1m` header and wait a tick.
- [ ] **alerting-strategies-33** (doc line 103-104) Lua crons live at `/cron.d/deploy-danger` (same name, same dir as bash) with `--` comment schedule syntax
  - Human: create Lua cron and `ls /cron.d/`.
- [ ] **alerting-strategies-34** (doc line 114-115) Pipe Lua cron stdout through `\| alert above 0 --notify=critical`, or wrap with bash `--if-zero`
  - Human: try `lua /cron.d/deploy-danger \| alert above 0`. Relies on alert position rules (22).
- [ ] **alerting-strategies-36** (doc line 120) `weft.latest("error-rate")` returns the latest value as string; `tonumber` works
  - Lua unavailable. Bash analog: `weft::latest error-rate` -> 125 (a plain number string).
- [ ] **alerting-strategies-37** (doc line 125) `io.stderr:write(string.format(...))` supported in cron Lua
  - Needs a Lua cron run and check of stderr handling.
- [ ] **alerting-strategies-53** (doc line 178-179) `tonumber("")` is nil in Lua
  - Lua unavailable under sh -c. Standard Lua: tonumber("") is nil (language fact, not verified in-stack).
- [ ] **alerting-strategies-54** (doc line 179-180) Arithmetic against nil errors
  - Lua unavailable. Standard Lua semantics.
- [ ] **alerting-strategies-55** (doc line 183) `tonumber(weft.latest("foo")) or 0` treats absent as zero
  - Lua unavailable. Also note bash `weft::latest` errors for missing cron (52), so Lua may differ.
- [ ] **alerting-strategies-59** (doc line 202-204) `alert --duration <dur>` requires trigger to hold for entire window before firing
  - Needs a 5m+ cron history. Accepted syntactically: `echo 1 \| alert above 0 --duration 5m` -> passthrough (not fired yet, as expected without history).
- [ ] **alerting-strategies-62** (doc line 215-218) `--duration` is per-tag for multi-row alerts; `alert above 80 -k 2 -t 1 --duration 5m` requires each host's 5m window to clear threshold
  - `printf "a 90\nb 10\n" \| alert above 80 -k 2 -t 1 --duration 5m` -> passthrough, no fire (no history). Real per-tag sustained windows need a cron over 5+ minutes.

## cloudwatch

Source: `docs/audit/cloudwatch.md`

### Terminal or browser

- [ ] **cw-06** (doc line 23-24) For sub-second tailing install weftd instead
  - open `man install-weftd` in a shell/web manual and confirm sub-second tailing claim (not run: `man` unavailable under sh -c)
- [ ] **cw-09** (doc line 30) Cross-ref to **Namespaces**
  - open Namespaces manual; static: corpus has `man namespaces` entry (help_corpus_gen.go line 60) and it mentions CloudWatch row
- [ ] **cw-30** (doc line 83) Interactive schema wizard shows real lines
  - Human step: `weft source configure myco/my-app` in a TTY after deploy; observe lines
- [ ] **cw-31** (doc line 83-85) Wizard asks where the timestamp is, which field holds log level, and whether tail is JSON payload
  - Same; note prompts
- [ ] **cw-32** (doc line 85-87) Auto-detects CloudWatch-flavored text (`2026-04-18 14:32:01 INFO {"user":"alice"}`) and pure-JSON Lambda logs
  - Same; ingest each shape, check suggestions
- [ ] **cw-40** (doc line 105-107) `weft source configure` needs a sample; low-traffic groups take a while
  - Human step: `weft source configure myco/my-app` right after add; observe message
- [ ] **cw-46** (doc line 5-7) Front matter: section connect-cloudwatch, category connect-source, order 40
  - Open web docs nav; static: generated connect-cloudwatch.ts exists and help corpus lists `man connect-cloudwatch`

### Write, account or human

- [ ] **cw-01** (doc line 12-14) Works for Lambda, ECS/Fargate, API Gateway, RDS or anything writing to a log group; no agent
  - 🔒 third-party AWS; human step: create a Lambda/ECS log group and deploy the stack; confirm no agent installed
- [ ] **cw-02** (doc line 14-16) A CloudFormation stack forwards a subscription filter through Kinesis Data Firehose directly to weft
  - 🔒 third-party AWS; human step: `aws cloudformation describe-stack-resources --stack-name weft-cloudwatch-firehose`
- [ ] **cw-03** (doc line 16-17) Nothing of weft's runs in your account; nothing polls; Firehose pushes
  - 🔒 third-party AWS; human step: inspect template Resources for any weft-owned compute
- [ ] **cw-05** (doc line 21-23) Latency is bounded by subscription-filter delivery plus Firehose buffering, roughly a minute at the lowest buffering setting in the template
  - 🔒 third-party AWS; human step: log a marker line in a subscribed group, time until `weft sh -c "tail ..."` shows it
- [ ] **cw-10** (doc line 31-32) Runnable from any machine with `weft` binary and valid login
  - needs clean machine + `weft login` (forbidden); `weft whoami` with token works from this binary
- [ ] **cw-12** (doc line 42) Command only records the mapping; nothing ingesting yet
  - Human step: run `weft source add ...` then `ls /log/myco` and expect no my-app dir
- [ ] **cw-15** (doc line 51-52) Deploy stack in the AWS account owning the log group
  - 🔒 third-party AWS; human step: deploy in the owning account
- [ ] **cw-16** (doc line 52-55) Stack provisions Firehose stream, IAM roles for CloudWatch Logs to write, subscription filter(s), and a required S3 bucket for undeliverable events
  - 🔒 third-party AWS; human step: `curl -s https://weft-public.s3.amazonaws.com/cfn/weft-cloudwatch-firehose.yaml` and inspect Resources (not fetched: S3/third-party)
- [ ] **cw-17** (doc line 54) S3 failure bucket has 30-day expiry
  - Human step: download template, check `ExpirationInDays: 30` on the failure bucket lifecycle
- [ ] **cw-18** (doc line 57-63) `aws cloudformation create-stack` invocation with `--stack-name weft-cloudwatch-firehose` and `--capabilities CAPABILITY_IAM`
  - 🔒 third-party AWS; human step: `aws cloudformation validate-template --template-url <url>`
- [ ] **cw-19** (doc line 59) Template URL `https://weft-public.s3.amazonaws.com/cfn/weft-cloudwatch-firehose.yaml` is publicly reachable
  - Human step: `curl -sI https://weft-public.s3.amazonaws.com/cfn/weft-cloudwatch-firehose.yaml` expect 200
- [ ] **cw-20** (doc line 60) Template parameter `WeftToken` exists
  - Human step: download template, grep `WeftToken` under Parameters
- [ ] **cw-21** (doc line 61) Template parameter `LogGroupNames` exists
  - Human step: same, grep `LogGroupNames`
- [ ] **cw-22** (doc line 62) `CAPABILITY_IAM` is sufficient (not CAPABILITY_NAMED_IAM)
  - 🔒 third-party AWS; human step: validate-template shows Capabilities; if template names IAM roles it needs CAPABILITY_NAMED_IAM
- [ ] **cw-24** (doc line 66-67) Token authenticates Firehose deliveries the same way any other push source does
  - Human step: deploy with token, check Firehose delivers (AWS)
- [ ] **cw-25** (doc line 67-68) `LogGroupNames` is comma-separated; one stack can subscribe several log groups in the same account/region
  - 🔒 third-party AWS; human step: inspect template parameter description/type
- [ ] **cw-26** (doc line 70-71) Updating `LogGroupNames` and redeploying adds/removes log groups without touching Firehose stream or IAM roles
  - 🔒 third-party AWS; human step: `update-stack` changeset review
- [ ] **cw-34** (doc line 89) Source starts ingesting as soon as you approve it
  - Human step: approve, then `weft source list` expect status configured/active
- [ ] **cw-37** (doc line 98-100) Firehose has its own CloudWatch log group created by the stack, separate from app's
  - 🔒 third-party AWS; human step: check stack resources for LogGroup for Firehose
- [ ] **cw-38** (doc line 100-101) Common cause of no rows: WeftToken doesn't match a live namespace token
  - 🔒 third-party AWS; human step: deploy with bogus token, view Firehose error log group
- [ ] **cw-39** (doc line 101-104) CloudWatch allows at most 2 subscription filters per log group
  - 🔒 third-party AWS; human step: `aws logs put-subscription-filter` third filter -> LimitExceededException
- [ ] **cw-42** (doc line 108-109) Wrong fields usually caused by schema `timestamp_format` being off
  - Human step: set wrong timestamp_format via configure, observe fields

## crons

Source: `docs/audit/crons.md`

### Terminal or browser

- [ ] **crons-03** (doc line 17) `vi /cron.d/error-count` opens the cron editor
  - Human: run `vi /cron.d/error-count` in interactive `weft sh`. Help corpus (static) also lists `vi /cron.d/<name>`.
- [ ] **crons-10** (doc line 25) Numeric values graphed on dashboard as sparklines
  - Browser: open Dashboard, confirm sparkline for numeric cron. (`ls -l /cron.d/` per help corpus adds a sparkline column.)
- [ ] **crons-13** (doc line 27-28) `man alert` documents builtin grammar
  - Human: run `man alert` in interactive weft sh. Static: help corpus HelpAlerts says "See man alert for grammar".
- [ ] **crons-15** (doc line 30) View and manage crons from the **Dashboard**
  - Browser: open Dashboard and confirm crons are listed with manage controls.
- [ ] **crons-29** (doc line 58-59) **Account** page shows current caps
  - Browser: open Account page and confirm cron count / min interval caps displayed.
- [ ] **crons-30** (doc line 59) `help cron` in shell shows tier limits
  - Human: run `help cron`; expect tier limits where `{{TIER_LIMITS}}` appears in corpus.
- [ ] **crons-32** (doc line 63-64) After `:w`, shell prompts for optional commit message
  - Interactive: `vi /cron.d/x`, edit, `:w`, confirm commit-message prompt appears.

### Write, account or human

- [ ] **crons-01** (doc line 12) Crons are saved pipelines that run on a schedule
  - Human: `vi /cron.d/audit-x`, save, wait 1 min, `weft::latest audit-x`. Read-only: existing cron error-rate has history (`weft::latest error-rate` -> 126, `weft::count error-rate 1h` -> 60), so scheduled runs do occur.
- [ ] **crons-02** (doc line 12-13) Create one from the shell
  - See crons-03.
- [ ] **crons-04** (doc line 19) Cron file format: `# schedule:` directive plus a pipeline
  - Existing `cat /cron.d/error-rate` -> `# schedule: */1 * * * *` then a pipeline, matching the shape. Saving is a write: human `vi`, `:w`.
- [ ] **crons-05** (doc line 20) `# schedule: */5 * * * *` is a valid schedule (5-field cron)
  - Needs write. Existing crons use `*/1 * * * *` form, so 5-field is accepted; `*/5` itself unsaved. Note help cron also documents `# schedule: every 1m`, which the doc omits.
- [ ] **crons-25** (doc line 48-49) Lua /bin/ script: `weft.avg("error-rate", "1h")` returns a number, no string parsing
  - Human: `vi /bin/rate.lua` with `local r = weft.avg("error-rate","1h"); print(type(r))`, run it, expect `number`. Existing `weft_script_propose` forbidden.

## dashboards

Source: `docs/audit/dashboards.md`

### Terminal or browser

- [ ] **dash-01** (doc line 12-13) A dashboard is a named view arranging existing crons into tiles
  - Web UI; dashboard files exist (`ls /etc/dashboards` -> prod.yaml)
- [ ] **dash-11** (doc line 47) counter `format: "%d jobs"` renders e.g. "42 jobs"
  - `format: "%d jobs"` accepted on counter (applied); rendering needs UI
- [ ] **dash-19** (doc line 72) counter: latest numeric value; requires `watch`
  - counter accepted; rendering needs UI. `weft::latest` returns 3
- [ ] **dash-20** (doc line 73) gauge: bar 0..threshold.max (default 100); requires `watch`
  - gauge accepted with/without max (q2 w/o threshold staged); rendering needs UI
- [ ] **dash-27** (doc line 85) `columns` for table: header labels; whitespace-split fields
  - `columns: [a, b]` accepted; header rendering needs UI. Cron `audit-dash-table` emitted `40 7e4ae977dce2 3`
- [ ] **dash-29** (doc line 90-92) Plain sparkline plots one scalar per run: the watch's last whitespace field
  - Cron emitted scalar; plot rule needs UI
- [ ] **dash-30** (doc line 92-95) Multi-row JSON stdout per run (one per tag value); `tag:` identifies row, `y:` field to plot; one line per distinct tag
  - JSONL cron `audit-dash-jsonl` emits one JSON row per memory event; `tag: .host`/`y: .pct` sparkline accepted; chart needs UI
- [ ] **dash-32** (doc line 105-107) Row's x value is its run's timestamp (not per-row timestamp)
  - x-axis behavior needs UI
- [ ] **dash-33** (doc line 107-108) Lines get a legend and a distinct color per tag
  - legend/colors need UI
- [ ] **dash-35** (doc line 110-113) Renderers (browser/terminal) map size hint to viewport
  - needs web + terminal render
- [ ] **dash-36** (doc line 112-113) Wide browser: `lg` tile spans full row; narrow terminal: single column
  - needs resize in browser/terminal
- [ ] **dash-38** (doc line 123-124) Values >= crit render red; >= warn and < crit yellow
  - color rendering needs UI
- [ ] **dash-39** (doc line 124-125) gauge uses threshold.max as full scale, default 100
  - gauge scale needs UI
- [ ] **dash-41** (doc line 129-131) Each section gets its own heading and grid
  - multi-section applied; headings need UI
- [ ] **dash-44** (doc line 146-148) Renderer flows tiles row-major, per viewport and size hint
  - UI
- [ ] **dash-45** (doc line 150-152) Browser CSS grid: 6 tracks wide, 2 on tablet, 1 on phone
  - needs browser devtools
- [ ] **dash-46** (doc line 151-152) sm spans 2 tracks, md 3, lg 6
  - needs browser devtools
- [ ] **dash-47** (doc line 153-154) Terminal: greedy row packing, kind-specific min widths
  - needs terminal render
- [ ] **dash-48** (doc line 154) Below ~60 columns terminal collapses to one tile per row
  - needs terminal resize
- [ ] **dash-49** (doc line 156-157) No drag-and-drop; no per-tile pixel positioning
  - UI
- [ ] **dash-50** (doc line 157) YAML order is screen order
  - UI
- [ ] **dash-51** (doc line 161) Tile headers link to cron's detail page `/watch/<name>` (legacy route name)
  - click needed
- [ ] **dash-52** (doc line 161-163) Detail page shows full history with zoom, range selection, per-tag legend
  - UI

## git

Source: `docs/audit/git.md`

### Terminal or browser

- [ ] **git-98** (doc line 192-193) Output to terminal is coloured
  - Human: run `git log -p` in the tty shell; expect coloured diff/headers. (Under `sh -c` stdout, `git log -p` and `git diff` emitted ANSI 38;5 colours.)
- [ ] **git-102** (doc line 199-201) After successful vi save of tracked file, shell opens follow-up vi buffer for commit message
  - Human: `vi /bin/t`, write, `:wq`; expect follow-up buffer.
- [ ] **git-103** (doc line 199-200) Applies to script, cron, dashboard, notify.yaml
  - Human: repeat for a cron, dashboard, /etc/notify.yaml.
- [ ] **git-104** (doc line 203-209) Buffer prefilled with `# Commit message for /bin/scan.` / `# One or more lines; first line is the subject shown in git log.` / `# Save & exit (:wq) to record...
  - Human: observe three `#` lines in the buffer.
- [ ] **git-105** (doc line 211) `:wq` records the message
  - Human: type msg, `:wq`; `git log --oneline` shows it first.
- [ ] **git-106** (doc line 211) `:q!` skips (generated message used)
  - Human: `:q!`; `git log --oneline` shows `Update ...`/`Create ...`.
- [ ] **git-107** (doc line 211-212) Either way returns to shell prompt
  - Human: observe prompt returns either way.
- [ ] **git-108** (doc line 212-214) `#` lines and blank lines stripped; all-comment buffer records no message
  - Human: `:wq` on unchanged buffer; check generated msg.
- [ ] **git-128** (doc line 4-8) Frontmatter: section git, category customize, order 65
  - Human: open web manual, check git section under Customize, order 65. Static: frontend/src/generated/docs/git.ts has section "git", category "customize", order 65.

### Write, account or human

- [ ] **git-01** (doc line 12-13) Every save to a script in /bin is captured in history
  - Human: `vi /bin/t`, :wq twice, then `git log --oneline -- /bin/t` shows 2 commits. Indirect: globex /bin has multi-commit histories (`git log --oneline -- /bin/scan` -> 3 commits).
- [ ] **git-02** (doc line 12-13) Every save to a cron in /cron.d is captured
  - Human: save /cron.d/t twice, `git log --oneline -- /cron.d/t`. Indirect: `git log --oneline -- /cron.d/error-rate` -> 3 commits.
- [ ] **git-03** (doc line 12-13) Every save to a dashboard in /etc/dashboards is captured
  - Human: save /etc/dashboards/d.yaml, `git log -- /etc/dashboards/d.yaml`. Indirect: `git log --oneline -- /etc/dashboards/prod.yaml` -> `137bab2 Create etc/dashboards/prod.yaml`.
- [ ] **git-04** (doc line 13) Every save to /etc/notify.yaml is captured
  - Human: `vi /etc/notify.yaml` save, `git log -- /etc/notify.yaml`. Indirect: log shows `c4ad907 Create etc/notify.yaml`.
- [ ] **git-36** (doc line 75-77) Heading "five-version cap"; latest six versions per file kept (live + five before)
  - Human: save /bin/t 8 times, `git log --oneline -- /bin/t` should show 6.
- [ ] **git-37** (doc line 77-78) Older versions are gone, no recycle bin
  - Human: after 8 saves `git show <first-rev>:bin/t` should fail `fatal: bad revision`.
- [ ] **git-38** (doc line 80-82) Oldest kept version is marked `grafted` in `git log`
  - Human: after 8 saves `git log -- /bin/t`, look for `grafted` on oldest. (`git log \| grep -c grafted` -> 0 here, no capped file.)
- [ ] **git-39** (doc line 82) Oldest kept version's patch shows the file as newly added
  - Human: `git log -p -- /bin/t`, oldest patch should be `new file`.
- [ ] **git-41** (doc line 83-84) Revisions older than cutoff fail with `fatal: bad revision`
  - Human: after 8 saves `git show <first-rev>`.
- [ ] **git-42** (doc line 84-85) Cutoff is per file, not one depth for the repo
  - Human: edit A 8 times, B 2 times; B should keep all.
- [ ] **git-60** (doc line 96) Example shows `Update cron.d/errors` generated for crons
  - Human: edit a cron, check `git log --oneline` for `Update cron.d/<name>`. Example in doc says `cron.d/errors`, a name not in globex.
- [ ] **git-75** (doc line 153-157) `git restore --source=HEAD~3 /bin/scan` makes old version current
  - Human: `git restore --source=HEAD~3 /bin/scan; cat /bin/scan`. Only refusals tested (outside-repo fatal, bad source -> `fatal: could not resolve nonexistent`).
- [ ] **git-76** (doc line 157) `git checkout HEAD~3 -- /bin/scan` is equivalent
  - Human: `git checkout HEAD~3 -- /bin/scan; cat /bin/scan`. `git checkout HEAD~1 -- /tmp/x` -> outside-repo fatal.
- [ ] **git-77** (doc line 160-161) Restore writes a NEW commit with message `Restore bin/scan from 0174c77`
  - Human: after restore `git log --oneline \| head -1` should read `Restore bin/scan from <7char>`.
- [ ] **git-78** (doc line 161) History never rewritten on restore
  - Human: after restore, `git log --oneline` retains older commits.
- [ ] **git-79** (doc line 161-163) Restore goes through same path as `vi`: permissions apply
  - Human: as user without binmgr run `git restore --source=HEAD~1 /bin/scan`, expect refusal.
- [ ] **git-80** (doc line 162) Lua/bash classification applies on restore
  - Human: restore version whose classification differs, check refusal/reclassification.
- [ ] **git-81** (doc line 162-163) For crons, plan's schedule limits apply on restore
  - Human: restore cron version with schedule below plan minimum; expect refusal.
- [ ] **git-82** (doc line 163-164) Restore brings back a deleted script if history still there
  - Human: `rm /bin/x`, then `git restore --source=<rev> /bin/x`, `cat /bin/x`.
- [ ] **git-84** (doc line 167-168) `git revert <rev>` undoes one commit with a new commit
  - Human: `git revert HEAD`, `git log --oneline`. Observed: `git revert` -> `usage: git revert [--no-edit] <commit>`; `git revert nonexistentrev` -> `fatal: bad revision 'nonexistentrev'`.
- [ ] **git-85** (doc line 168-169) Revert conflicts reported and nothing written
  - Human: revert an older commit of a file changed since; expect conflict and no write.
- [ ] **git-86** (doc line 172-175) Conflict output: `error: could not revert 3d2e91f... tighten error pattern` + `hint: bin/scan has changed since that commit, so the revert would conflict; no...
  - Human: as git-85; expect `error: could not revert <hash>... <subject>` + `hint: bin/scan has changed since that commit, so the revert would conflict; nothing was written`.
- [ ] **git-87** (doc line 177-178) `git commit --amend -m <message>` rewords latest commit
  - Human: `git commit --amend -m "x"; git log --oneline`. Observed: `git commit --amend` -> `error: 'git commit --amend' needs -m <message> here (there is no editor)`; `--amend -m` (no value) -> `error: switch `m' requires a value`.
- [ ] **git-88** (doc line 179-180) Can reword only a file's current version
  - Human: amend a commit whose file has been superseded; expect refusal.
- [ ] **git-89** (doc line 180) Can reword only one you wrote yourself
  - Human: amend a teammate's commit; expect refusal.
- [ ] **git-90** (doc line 181) Requires permission to edit that file
  - Human: amend as user lacking group.
- [ ] **git-91** (doc line 181-182) Cannot reword teammate's save that landed after yours
  - As git-89.
- [ ] **git-97** (doc line 188) rev-parse usable inside /bin scripts
  - Human: create script containing `git rev-parse HEAD`, run it. (`git rev-parse HEAD` in `sh -c` works.)
- [ ] **git-109** (doc line 214-215) `git commit --amend -m` adds message later
  - Human: `git commit --amend -m 'msg'` after skipping popup.
- [ ] **git-110** (doc line 219-220) Deleting a script or cron shows as `Delete bin/<name>` commit
  - Human: `rm /bin/t`; `git log --oneline` -> `Delete bin/t`.
- [ ] **git-111** (doc line 220-221) `git restore --source=<rev> <path>` brings deleted file back with history intact
  - Human: restore after rm; `git log -- bin/t` keeps history.
- [ ] **git-112** (doc line 221-223) Saving new file over deleted name: deletion stays in log followed by new save
  - Human: rm then recreate same name; `git log`.
- [ ] **git-113** (doc line 227-228) `git log`, `show`, `diff`, `status`, `blame`, `rev-parse` are read-only, same access as `cat /bin/foo` / `cat /cron.d/foo`
  - Human: as read-only user run each.
- [ ] **git-114** (doc line 230-232) `restore`, `checkout -- <path>`, `revert`, `commit --amend` are writes
  - Needs read-only user.
- [ ] **git-115** (doc line 232) Need `binmgr` for /bin
  - Needs user lacking binmgr (dana has binmgr cronmgr configmgr per `cat /etc/passwd`).
- [ ] **git-116** (doc line 232) Need `cronmgr` for /cron.d
  - Needs user lacking cronmgr.
- [ ] **git-117** (doc line 232) Need `configmgr` for /etc
  - Needs user lacking configmgr.
- [ ] **git-118** (doc line 233-234) Restoring dashboard referencing deleted cron is refused
  - Human: delete cron referenced by a dashboard, restore old dashboard version; expect refusal.
- [ ] **git-124** (doc line 245) Team changed with `adduser` / `deluser`
  - Human: `adduser`, `deluser` in shell.
- [ ] **git-125** (doc line 246-247) Slack setup not in /etc/notify.yaml; connecting Slack creates no commit
  - Needs Slack account.
- [ ] **git-126** (doc line 247) Only the alias file is versioned
  - Human: edit alias in /etc/notify.yaml, `git log -- /etc/notify.yaml`. notify.yaml is tracked (c4ad907).

## help-alerts

Source: `docs/audit/help-alerts.md`

### Terminal or browser

- [ ] **help-alerts-02** (doc line 8-11) Web docs: section alerting, category alert-visualize, order 70
  - web UI nav step needed
- [ ] **help-alerts-21** (doc line 49-51) See `docs/manual/alerting-strategies.md` and `docs/manual/weft-funcs.md`
  - web UI pages needed; also doc names repo paths, not page titles
- [ ] **help-alerts-22** (doc line 49-51) alerting-strategies has seasonal/outlier guidance; weft-funcs has per-function reference
  - read in web UI
- [ ] **help-alerts-32** (doc line 77-79) `vi /etc/notify.yaml` edits it
  - `vi: command not found` in sh -c; and writes to /etc/notify.yaml (`>>`, `cp`, `tee`) -> `/etc: permission denied (need the configmgr group)` although whoami/passwd list alice with configmgr

### Write, account or human

- [ ] **help-alerts-05** (doc line 18-19) In a cron, stderr drives incidents and notifications
  - cron creation denied (`/cron.d: permission denied (need the cronmgr group)` even though whoami lists cronmgr); proposals need user approval, not applied
- [ ] **help-alerts-10** (doc line 26) `changed` fires when value differs from previous run
  - needs cron (previous-run state); cron creation blocked. One-shot `echo 60 \| alert changed` -> 60, no fire
- [ ] **help-alerts-33** (doc line 81-83) Four service types: slack, pagerduty, webhook, email; all are lists; entries bare string or mapping
  - could not edit notify.yaml (permission denied, see 32); not tested with each service type. Existing file uses webhook and email forms
- [ ] **help-alerts-34** (doc line 85-110) YAML example (ops, critical) valid and accepted
  - could not write the example to /etc/notify.yaml (permission denied)
- [ ] **help-alerts-35** (doc line 112) Slack: `"#ops"` or `{channel: "#name"}`
  - blocked, see 32
- [ ] **help-alerts-36** (doc line 113-114) PagerDuty: integration names, severity defaults `error`
  - needs PagerDuty and notify.yaml edit; blocked
- [ ] **help-alerts-37** (doc line 114) PagerDuty `{name, severity}`; severity one of critical/error/warning/info
  - blocked: save/validation path unreachable
- [ ] **help-alerts-39** (doc line 116) Webhook POSTs JSON payload with keys `v`, `watch`, `alias`, `ts_unix`, `body`
  - could not point an alias at a local listener: notify.yaml not writable; existing aliases target unresolvable hooks.loom.test (`lookup hooks.loom.test ... no such host`)
- [ ] **help-alerts-40** (doc line 116) Payload key is `watch` (not `cron`)
  - same as 39
- [ ] **help-alerts-41** (doc line 117-118) Email: bare address or `{address}`; plain text email, subject is cron name
  - no inbox; email not exercised
- [ ] **help-alerts-44** (doc line 125) `weft integration slack connect`
  - `weft integration slack connect` -> `start slack connect: HTTP 503: Slack integration not configured`
- [ ] **help-alerts-47** (doc line 130) PagerDuty integration name must match one registered
  - needs notify.yaml edit; blocked
- [ ] **help-alerts-51** (doc line 140-141) Interactive runs don't open an incident
  - no incident listing command reachable; unverified

## help-builtins

Source: `docs/audit/help-builtins.md`

### Terminal or browser

- [ ] **hb-01** (doc line 2-7) `help builtins` is the topic that displays this page
  - Human: run `help builtins` in interactive weft.
- [ ] **hb-02** (doc line 9-11) Web docs section `builtins`, category query, order 60
  - Browser: open docs nav, confirm Builtins under Query, position 60. Static: frontend/src/generated/docs/builtins.ts exists; &lt;/&gt; there are valid HTML escapes.
- [ ] **hb-06** (doc line 18) `tail -f` follows
  - Human: in interactive weft run `tail -f /log/acme/prod/api/main.log` while ingesting; expect new lines to stream.
- [ ] **hb-19** (doc line 33) `man less` page exists
  - Human: `man less`. Note: since less is a pass-through, man page content may be misleading.
- [ ] **hb-27** (doc line 40) `man chart` exists
  - Human: `man chart`.
- [ ] **hb-38** (doc line 46) `vi <path>` edits with vim keys
  - Human: interactive `vi /tmp/x`, type, `:wq`, `cat /tmp/x`.
- [ ] **hb-59** (doc line 75) Teammates open link in their shell pre-filled
  - Open the short link as a second user in a browser/shell, confirm script and range prefilled.
- [ ] **hb-60** (doc line 75) `man clipboard` exists
  - Human: `man clipboard`.
- [ ] **hb-72** (doc line 88) `man passwd` exists
  - Human: `man passwd`.
- [ ] **hb-74** (doc line 92) `help alerts` exists
  - Human: `help alerts`.
- [ ] **hb-75** (doc line 92) `man alert` exists
  - Human: `man alert`.
- [ ] **hb-82** (doc line 103) `man <name>` exists wherever a page exists for every entry
  - Human: loop `man <cmd>` for each command.
- [ ] **hb-83** (doc line 16-19) Command list omits `man`, `help`, `pwd`, `echo`, `export`, etc.
  - Human: compare `help` output.

### Write, account or human

- [ ] **hb-34** (doc line 44) `touch` resets the 48h clock on `/tmp` file
  - `touch /tmp/audit-hbuiltins-a` accepted; `ls -l` shows no expiry column or mtime change within same minute (12:12). Reset of the 48h clock cannot be observed: human step: `touch /tmp/x`, wait 47h, touch again, check it survives 48h after first touch.
- [ ] **hb-35** (doc line 44) `/tmp` files expire after 48h
  - 48h expiry needs a 48h wait. Step: create /tmp/x, return after 48h, `ls /tmp`. `ls -l /tmp` shows only mtime (YYYY-MM-DD HH:MM), no expiry info.
- [ ] **hb-51** (doc line 65) `git restore --source=<rev> <path>` makes past version current as new commit
  - Writes a commit. Step: `git restore --source=HEAD~3 /bin/scan` as binmgr user in interactive shell, then `git log --oneline -- /bin/scan` expects a new top commit.
- [ ] **hb-52** (doc line 66) `git revert <rev>` undoes one commit with new commit
  - Writes a commit. Step: `git revert HEAD` then `git log --oneline \| head -2`.
- [ ] **hb-53** (doc line 67) `git commit --amend -m MSG` rewords latest commit
  - Writes. Step: `git commit --amend -m "reworded"` then `git log --oneline \| head -1`.
- [ ] **hb-56** (doc line 70) Last 5 past versions retained
  - Needs 7 writes to one file. Step: edit /bin/x 7 times, then `git log --oneline -- /bin/x \| wc -l`, expect 6 (5 past + current). Existing /bin/scan has only 3 commits so cannot test. Note `git log --oneline \| wc -l` for whole repo is 31, so global history is not capped at 5.
- [ ] **hb-62** (doc line 77) "Team (owners)" section: commands available to owners
  - No non-admin user in acme/globex. Step: log in as a non-owner member (e.g. in a third org) and run `cat /etc/passwd`.
- [ ] **hb-63** (doc line 80) `adduser <email>` invites to org, sends invite email
  - Sends email / mutates org; forbidden. Step: `adduser test@example.com`, check inbox.
- [ ] **hb-64** (doc line 81) `adduser <email> <group>` adds member to group
  - Mutating. Step: `adduser a@x.com binmgr; groups a@x.com`.
- [ ] **hb-65** (doc line 81) `deluser` removes (from group form)
  - Mutating; also doc says only "`deluser` to remove" without syntax. Step: `deluser a@x.com binmgr`; expect group removed but still in org.
- [ ] **hb-66** (doc line 82) `deluser <email>` removes from org
  - Mutating. Step: `deluser a@x.com; cat /etc/passwd`.
- [ ] **hb-68** (doc line 85-86) Groups: `binmgr`, `cronmgr`, `configmgr` edit `/bin`, `/cron.d`, `/etc` respectively
  - Needs adduser. Step: `adduser u@x.com cronmgr`, as u edit /cron.d (allowed) and /bin (denied). Observed: alice has binmgr cronmgr configmgr plus undocumented `nsmgr`.
- [ ] **hb-69** (doc line 86-87) Namespace-name group grants read of its logs
  - Needs adduser and second login. Step: add user to group `acme`, `ls /log/acme` works, other ns fails.
- [ ] **hb-70** (doc line 87) `<ns>+tokens` group lets user mint producer tokens
  - Needs adduser and token create. Step: `adduser u@x.com acme+tokens`; `weft token create --namespace acme` as u. alice has `acme+tokens` in /etc/passwd output (consistent).

## help-cron

Source: `docs/audit/help-cron.md`

### Terminal or browser

- [ ] **hcron-01** (doc line 7) `help cron` shows this page
  - Human: run `weft sh`, type `help cron`.
- [ ] **hcron-06** (doc line 14) `help alerts` documents alerts
  - `help alerts` not available under sh -c. Static: HelpAlerts const exists in help_corpus_gen.go:6.
- [ ] **hcron-12** (doc line 19) `man chart` exists
  - `man chart` -> "man: command not found" under sh -c. Static: ManChart presence not checked; human: `man chart`.
- [ ] **hcron-29** (doc line 36) `man weft` is the full reference
  - Human: `man weft`.
- [ ] **hcron-32** (doc line 39) `alert above 50` syntax
  - `man alert` not available. Syntax `alert above 50` accepted: `echo 5 \| alert above 50 --notify=ops` echoed 5 silently (no fire); `echo 99 \| alert above 50 ...` fired.
- [ ] **hcron-35** (doc line 42-44) A Tier limits section lists tier limits
  - Human: `help cron` in an interactive shell and confirm a table.
- [ ] **hcron-37** (doc line 17 of help-index) index says `help cron` covers scheduled crons, result accessors, tier limits
  - Human: `help cron`.

### Write, account or human

- [ ] **hcron-02** (doc line 12) A cron runs a pipeline on a schedule
  - Human: `vi /cron.d/t`, save, wait a minute, `chart t`.
- [ ] **hcron-05** (doc line 14) Stderr alert firing applies only if cron has a `\| alert` stage
  - Needs a persistent cron without `\| alert`: `vi /cron.d/t` with `echo x >&2`, wait, check alerts.
- [ ] **hcron-07** (doc line 16) `vi /cron.d/<name>` creates or edits a cron
  - Human: `vi /cron.d/t`, enter `# schedule: every 1m` + `echo 5`, `:wq`.
- [ ] **hcron-13** (doc line 20) `rm /cron.d/<name>` deletes cron and stops schedule
  - Human: `rm /cron.d/t`, then `ls /cron.d` and confirm no new results.
- [ ] **hcron-15** (doc line 25) `# schedule: every 1m` is valid syntax
  - Human: save `# schedule: every 1m` in /cron.d/t and see if accepted.
- [ ] **hcron-17** (doc line 26) `# notify: ops` header sets alias from /etc/notify.yaml
  - Human: add `# notify: ops` to a cron, trigger alert; dispatch goes to webhook/email (third party).

## help-index

Source: `docs/audit/help-index.md`

### Terminal or browser

- [ ] **hidx-17** (doc line 32) Long output is paged automatically
  - type a long command (e.g. `cat /log/acme/prod/api/main.log \| tail 500`) in interactive `weft sh`; confirm pager. Under sh -c output streams unpaged (tail 100 printed all lines).
- [ ] **hidx-18** (doc line 32) Press `q` to return from pager
  - in interactive `weft sh`, run long output, press q, expect prompt.

## heroku

Source: `docs/audit/heroku.md`

### Terminal or browser

- [ ] **heroku-07** (doc line 23) Web UI: Namespaces -> your namespace -> Create token
  - Web: open Namespaces, pick a namespace, look for a "Create token" button.
- [ ] **heroku-23** (doc line 63) Cross-reference to **Namespaces** doc for service-name selection
  - Static: `man namespaces` is in the man index (shell/help_corpus_gen.go ManIndex lists `man namespaces`); run `man namespaces` in interactive shell and confirm it covers Heroku service naming. Doc says **Namespaces** (no `man` hint).
- [ ] **heroku-32** (doc line 77) Cross-reference to **Namespaces** for layout rules
  - `man namespaces` interactively; confirm layout rules.
- [ ] **heroku-56** (doc line 5-7) Front matter: section connect-heroku, category connect-source, order 35
  - Static: man index lists `man connect-heroku` (ManIndex) and shell/help_corpus_gen.go has `"man connect-heroku": ManualConnectHeroku`. Web: confirm section order between 30 and 37 in connect-source category.

### Write, account or human

- [ ] **heroku-01** (doc line 12) weft accepts logs directly from Heroku's HTTPS log-drain protocol
  - Human: `heroku drains:add <url> -a app`, then `tail` the app files.
- [ ] **heroku-02** (doc line 13) No agent, no AWS account, no schema file required
  - End-to-end needs Heroku; nothing observable locally.
- [ ] **heroku-03** (doc line 13-14) You mint a token, point a drain at weft, dynos flow "within seconds"
  - Timing: run `heroku drains:add`, time until first row in `weft sh -c "tail -n 1 /log/<ns>/<env>/<app>/web.log"`.
- [ ] **heroku-05** (doc line 22) Each token is scoped to a single namespace
  - Human: mint `--namespace a`, POST to ingest, confirm rows only under a.
- [ ] **heroku-13** (doc line 32-33) Separate envs need separate tokens because env is bound at mint time
  - Mint `--env prod` token, POST frames with two appnames, confirm all land under /log/<ns>/prod/ only. (create command prints `Env: prod (stamped on every row from this token)` per source, static.)
- [ ] **heroku-14** (doc line 37) Command `heroku drains:add <url> -a <app>` is how to add a drain
  - Heroku CLI/account.
- [ ] **heroku-18** (doc line 46) Heroku starts delivering within a few seconds
  - Needs Heroku.
- [ ] **heroku-22** (doc line 62) Each (appname, procid) pair becomes its own weft sub-app
  - POST frames with differing procids to a valid token, `ls`. Adapter comment (api/adapter_heroku.go:19) says each (appname, procid) becomes a sub-app, consistent with doc; not black-box verified.
- [ ] **heroku-24** (doc line 64-65) Dyno number (.1, .2) dropped from app name and stamped on `instance`
  - Needs valid-token POST of web.1/web.2 frames, then `cat .../web.log \| jq .instance`.
- [ ] **heroku-25** (doc line 65-66) A single `tail /log/.../web.log` covers every web dyno
  - Same as 24.
- [ ] **heroku-26** (doc line 70) `myapp web.1`, `web.2` -> `/log/<ns>/myapp/web.log` with `instance=web.1`
  - Same as 24; also check instance is `web.1` not `1`.
- [ ] **heroku-27** (doc line 71) `myapp worker.1` -> `/log/<ns>/myapp/worker.log`
  - POST worker.1 frame then ls.
- [ ] **heroku-28** (doc line 72) `myapp router` -> `/log/<ns>/myapp/router.log`
  - POST router frame then ls.
- [ ] **heroku-29** (doc line 73) `myapp heroku-postgres` -> `/log/<ns>/myapp/heroku-postgres.log`
  - POST heroku-postgres frame then ls.
- [ ] **heroku-30** (doc line 74) Platform events (deploys, restarts, scales) -> `/log/<ns>/myapp/platform.log`
  - POST appname=heroku frame then ls shows platform.log.
- [ ] **heroku-33** (doc line 79) Metric lines auto-route to `/metrics`
  - POST l2met line, ls. Real tree has `metrics.log` per service (`ls /log/acme/prod/api` -> main.log metrics.log).
- [ ] **heroku-34** (doc line 81) `runtime-heroku-metrics` labs feature emits l2met-shaped lines
  - Heroku labs feature.
- [ ] **heroku-35** (doc line 85) Example line `source=web.1 sample#memory_total=512.00MB sample#memory_rss=487.21MB sample#load_avg_1m=0.42` is recognised as metric
  - POST with valid token and check metrics.log.
- [ ] **heroku-37** (doc line 88-89) key/value pairs lifted into payload JSON
  - `tail /log/<ns>/<env>/myapp/metrics.log \| jq .` after POST.
- [ ] **heroku-38** (doc line 89-90) Numeric where value is numeric (`512.00MB` -> number 512)
  - jq type check after POST.
- [ ] **heroku-39** (doc line 90-91) Trailing units like MB preserved on sibling `<key>_unit` field
  - jq .memory_total_unit after POST. Static: grep for `_unit` in api/ingest Go found nothing conclusive (glob failed); verify by running.
- [ ] **heroku-40** (doc line 91-93) Original line still flows through the dyno's pseudofile; `grep memory_total /log/.../web.log` works
  - grep after POST.
- [ ] **heroku-41** (doc line 96-97) `heroku labs:enable "runtime-heroku-metrics" -a myapp` enables metric stream
  - Heroku CLI.
- [ ] **heroku-44** (doc line 105) `heroku drains -a myapp` has a `Last error` column
  - Heroku CLI.
- [ ] **heroku-49** (doc line 111-112) Weft maps platform events to `<your-app>/platform`
  - POST appname=heroku frame; adapter code comment says it overrides procid to platform; not verified black-box.
- [ ] **heroku-50** (doc line 112-114) Unseen add-on procid appears automatically under `/log/<ns>/<your-app>/<procid>.log`
  - POST novel procid.
- [ ] **heroku-51** (doc line 115) Default cap is 1000 events/sec per token
  - Load test against valid token; do not run on shared stack.
- [ ] **heroku-52** (doc line 115) Over-cap drains see 429 responses
  - Same as 51.
- [ ] **heroku-54** (doc line 118-119) Split into more tokens to exceed cap (cap is per token)
  - Same as 51 with two tokens.

## man-alert

Source: `docs/audit/man-alert.md`

### Terminal or browser

- [ ] **man-alert-01** (doc line 7-9) `man alert` and `alert --help` both show this page; banner ALERT(1)
  - Human: type `man alert` and `alert --help` in `weft sh`.
- [ ] **man-alert-32** (doc line 86-89) Predicate builtins `weft::is_above`, `weft::is_absent` etc.; see `man weft`
  - Human: `man weft` and confirm it lists them.
- [ ] **man-alert-60** (doc line 136) SEE ALSO: `help`, `help alerts`, `vi`, `cat` exist
  - Human: `help alerts`, `man vi`, `man cat`. Note "SEE ALSO" says `vi` not `man vi`.

### Write, account or human

- [ ] **man-alert-07** (doc line 27-29) With `--notify`, dispatches alias from `/etc/notify.yaml` to Slack, PagerDuty, webhook, email
  - Human: configure real alias and check Slack/PagerDuty/email receipt.
- [ ] **man-alert-10** (doc line 36-37) In a cron, incidents deduplicated on (cron, condition)
  - Human: create cron with two alerts of different conditions, observe 2 incidents; same condition 1.
- [ ] **man-alert-11** (doc line 37-38) Subsequent ticks during open incident update it, don't re-page
  - Needs a cron over multiple ticks; check incident updates and one notification.
- [ ] **man-alert-12** (doc line 38-39) When condition clears, incident closes
  - Needs a cron; drop value below threshold and check incident status closed.
- [ ] **man-alert-18** (doc line 56-58) Each distinct tag opens/closes incidents and dispatches independently
  - Human: cron with two tags, one clears.
- [ ] **man-alert-33** (doc line 87-88) Composition with bash `&& \|\| !` or Lua `and or not`; pass exit code via `--if-zero "$?"`
  - `weft::is_above error-rate 50 && weft::is_absent canary-ping` ran silently with no error. Full composition + `--if-zero "$?"` needs a cron step: write the 2 lines to /cron.d/audit-test via `vi` and watch tick.
- [ ] **man-alert-45** (doc line 111) `--duration <dur>` above/below: must hold for ALL runs in window
  - Human: run cron for >window and verify only fires when all runs exceed.
- [ ] **man-alert-46** (doc line 111) `--duration` rising/falling: compares against window average
  - Needs cron history; single run silent.
- [ ] **man-alert-49** (doc line 112) `--average`: for above/below with `--duration`, compare rolling average instead of every run
  - `--average --duration 5m` accepted (silent single run); semantics need cron history.

## man-chart

Source: `docs/audit/man-chart.md`

### Terminal or browser

- [ ] **man-chart-01** (doc line 7-8) `man chart` and `chart --help` show page; banner CHART(1)
  - Human: run `man chart` and `chart --help` in `weft sh`.
- [ ] **man-chart-08** (doc line 32-34) `--follow` live redraw every INTERVAL until q
  - Human: in `weft sh` run `chart error-rate --follow`, watch it repaint every ~1s, press q.
- [ ] **man-chart-09** (doc line 33-34) Also exits on Esc, Ctrl+C, Ctrl+D
  - Human: `chart error-rate -f` then try q, Esc, Ctrl+C, Ctrl+D in CLI and in the web shell; each should return to prompt.
- [ ] **man-chart-14** (doc line 42-43) Charts self-fit terminal; never paged through less
  - Human: run `chart error-rate` in a short and tall terminal; chart should fill without `less`. Under sh -c no pager appeared.
- [ ] **man-chart-15** (doc line 43) Resize re-fits in cron `--follow` on next tick
  - Needs TTY: run `chart error-rate -f`, resize window, confirm re-fit within one tick.
- [ ] **man-chart-16** (doc line 43-45) Resize in non-follow/pipe mode: not stated (no re-fit)
  - Human: run `chart error-rate`, resize, observe no re-fit.
- [ ] **man-chart-17** (doc line 44-45) No zoom or scroll
  - Needs TTY: press arrows / scroll during `chart error-rate -f`; nothing should happen.
- [ ] **man-chart-24** (doc line 59-60) `-i`/`--interval` DURATION redraw interval in follow; default `1s`
  - Human: `chart error-rate -f` and watch tick at ~1s; try `-i 5s`.
- [ ] **man-chart-26** (doc line 61-63) `-f`/`--follow` re-fetches and repaints every interval until q
  - Needs TTY and a new cron result mid-follow (crons run every minute): `chart error-rate -f`, wait one minute, confirm new point appears.
- [ ] **man-chart-35** (doc line 79-80) Each line gets own color and legend row
  - Human: run `chart` with --tag in terminal and confirm distinct colors.
- [ ] **man-chart-45** (doc line 95) `chart error-rate --follow` live; press q
  - See 08; under sh -c follow is not interactive.
- [ ] **man-chart-50** (doc line 116-117) `ls -l /cron.d/` adds colored sparkline beside each cron name
  - Human: run `ls -l /cron.d/` in `weft sh` and look for colored sparkline.
- [ ] **man-chart-53** (doc line 42) Charts never run through less, even for large `-n`
  - `chart error-rate -n 1000` gave one frame, "(last 50)", no pager under sh -c. Paging only matters on TTY.

## man-clipboard

Source: `docs/audit/man-clipboard.md`

### Terminal or browser

- [ ] **man-clipboard-02** (doc line 9) Banner is `CLIPBOARD(1)`
  - static: corpus string begins "CLIPBOARD(1) Weft Commands CLIPBOARD(1)"; live render needs `man clipboard` in interactive shell
- [ ] **man-clipboard-08** (doc line 27-28) Records the current shell's view of a `/bin/` script
  - link created, but verifying the stored view needs opening it in the web UI: click the printed URL in a signed-in browser (the local server returns 404 for GET /s/<token>, so it is a frontend route) and compare script/range
- [ ] **man-clipboard-11** (doc line 29-31) Teammate signed in to same org opening link lands in the web shell
  - Open the printed link in a second browser signed in as bob (acme); expect web shell
- [ ] **man-clipboard-12** (doc line 30-31) Script invocation is pre-filled in prompt, run with a single Enter
  - Open link in own signed-in browser; check prompt is pre-filled with `scan`
- [ ] **man-clipboard-13** (doc line 31-32) Anyone outside the org gets "that link doesn't exist"
  - Open link in logged-out browser or as dana (globex); expect "that link doesn't exist"
- [ ] **man-clipboard-15** (doc line 35) Stores script path `/bin/<name>`
  - Open link in web shell; prompt should hold script invocation
- [ ] **man-clipboard-16** (doc line 36) Stores time range used for log queries
  - Open link from `--range=2h..` (.../s/ntKXQNxFiZ) and run `range`; expect 2h window
- [ ] **man-clipboard-17** (doc line 37-38) Stores args typed after `--`
  - Open .../s/eKfVrA6RSf; prompt should hold `scan --app=loom --since=1h`
- [ ] **man-clipboard-19** (doc line 42-43) Recipient can edit before Enter
  - Open link, edit text before Enter
- [ ] **man-clipboard-20** (doc line 43) Nothing executes automatically
  - Open link, confirm nothing runs until Enter
- [ ] **man-clipboard-28** (doc line 56-57) Everything after `--` becomes script argument vector, joined with spaces
  - Link created for `-- a b` (.../s/MAvpMYI0tj); open it and read prompt: expect `scan a b`
- [ ] **man-clipboard-29** (doc line 57-58) Whitespace and quoting not preserved beyond simple space separation
  - Link created for `-- "a b" c` (.../s/ou0qac9Cvl); open it, expect prompt `scan a b c`. Note the link differs from `-- a b` (MAvpMYI0tj vs ou0qac9Cvl), so quoting is apparently preserved in storage, contradicting "not preserved"; confirm in UI
- [ ] **man-clipboard-31** (doc line 63-64) iTerm2, kitty, WezTerm, xterm with OSC52 enabled, and weft web shell write to clipboard
  - Terminal/emulator behavior: run in iTerm2, kitty, WezTerm, web shell, then paste
- [ ] **man-clipboard-33** (doc line 69-71) Over SSH OSC-52 still works; local terminal does clipboard write
  - Run over SSH from a local OSC-52 terminal and paste locally
- [ ] **man-clipboard-35** (doc line 79) `clipboard /bin/error-rate -- --app=loom --since=1h` pre-fills `error-rate --app=loom --since=1h`
  - Syntax accepted (`-- --app=loom --since=1h` -> link .../s/eKfVrA6RSf); open link to see pre-fill. /bin/error-rate absent here
- [ ] **man-clipboard-36** (doc line 79-80) Prefilled command text drops `/bin/` prefix (shown as `error-rate ...`)
  - Open link from 35; check prompt starts `error-rate`/`scan` without /bin/
- [ ] **man-clipboard-37** (doc line 82-84) `clipboard /bin/scan --range=24h..` pins 24-hour window regardless of sharer's range
  - `clipboard /bin/scan --range=24h..` -> link .../s/wyecnPHDWO accepted; open it and run `range`. Note `24h..` is accepted
- [ ] **man-clipboard-40** (doc line 87-88) `prod` passed as one positional arg
  - Open .../s/SMHXLIca9Y; expect prompt `scan prod`

### Write, account or human

- [ ] **man-clipboard-21** (doc line 43-45) Scripts resolve at HEAD on recipient's side; no version pinning
  - Needs a persistent write: share link, edit /bin/scan (`vi /bin/scan`), reopen link in browser, confirm new version runs

## man-dashboard

Source: `docs/audit/man-dashboard.md`

### Terminal or browser

- [ ] **man-dashboard-03** (doc line 14) `dashboard` views a named tile layout in the terminal
  - `dashboard` is not a command under `sh -c` (`dashboard nonexist` -> `bash: dashboard: command not found`; `which dashboard` -> `no dashboard in PATH`). Human step: in a real `weft sh` TTY type `dashboard` (expect list of names) then `dashboard prod`
- [ ] **man-dashboard-04** (doc line 19) `dashboard` with no args is valid
  - `dashboard` is not a command under `sh -c` (`dashboard nonexist` -> `bash: dashboard: command not found`; `which dashboard` -> `no dashboard in PATH`). Human step: in a real `weft sh` TTY type `dashboard` (expect list of names) then `dashboard prod`
- [ ] **man-dashboard-05** (doc line 25) No args lists available dashboards
  - `dashboard` is not a command under `sh -c` (`dashboard nonexist` -> `bash: dashboard: command not found`; `which dashboard` -> `no dashboard in PATH`). Human step: in a real `weft sh` TTY type `dashboard` (expect list of names) then `dashboard prod`
- [ ] **man-dashboard-06** (doc line 20,27) `dashboard <name>` enters interactive full-screen view
  - `dashboard` is not a command under `sh -c` (`dashboard nonexist` -> `bash: dashboard: command not found`; `which dashboard` -> `no dashboard in PATH`). Human step: in a real `weft sh` TTY type `dashboard` (expect list of names) then `dashboard prod`
- [ ] **man-dashboard-07** (doc line 27-28) Arrow keys move focus between tiles
  - Human: `dashboard prod` in a TTY, press arrows / Enter / Esc, Esc
- [ ] **man-dashboard-08** (doc line 28-29) Enter zooms the focused tile to full screen
  - Human: `dashboard prod` in a TTY, press arrows / Enter / Esc, Esc
- [ ] **man-dashboard-09** (doc line 29) Esc exits zoom, or the view when not zoomed
  - Human: `dashboard prod` in a TTY, press arrows / Enter / Esc, Esc
- [ ] **man-dashboard-16** (doc line 41) Tile kind `sparkline`
  - Human: save a dashboard with a sparkline tile on an existing cron (`error-rate` or `mem-pressure`), view with `dashboard <name>`. Existing prod.yaml uses sparkline/counter/gauge kinds (`cat /etc/dashboards/prod.yaml`) so these kinds exist
- [ ] **man-dashboard-18** (doc line 43) `size: md`
  - Layout needs UI; `size: lg/sm` appear in prod.yaml but rendering unseen
- [ ] **man-dashboard-19** (doc line 44) Tile kind `counter`
  - Human: save a dashboard with a counter tile on an existing cron (`error-rate` or `mem-pressure`), view with `dashboard <name>`. Existing prod.yaml uses sparkline/counter/gauge kinds (`cat /etc/dashboards/prod.yaml`) so these kinds exist
- [ ] **man-dashboard-21** (doc line 47) `format: "%d jobs"` renders e.g. "42 jobs"
  - Human: counter on a cron that outputs 42 with `format: "%d jobs"`; expect `42 jobs`
- [ ] **man-dashboard-22** (doc line 49) Tile kind `table`
  - Human: save a table tile with `columns:` on a cron emitting whitespace-split rows and view
- [ ] **man-dashboard-23** (doc line 51) `columns: [host, count]`
  - Header rendering needs UI
- [ ] **man-dashboard-24** (doc line 52) `size: lg`
  - Render needs UI
- [ ] **man-dashboard-25** (doc line 54-58) Tile kind `gauge` with `threshold: { warn: 80, crit: 95, max: 100 }`
  - Human: save a dashboard with a gauge tile on an existing cron (`error-rate` or `mem-pressure`), view with `dashboard <name>`. Existing prod.yaml uses sparkline/counter/gauge kinds (`cat /etc/dashboards/prod.yaml`) so these kinds exist
- [ ] **man-dashboard-31** (doc line 71) Section `title` optional, rendered above tiles
  - Rendering needs UI
- [ ] **man-dashboard-38** (doc line 78) `size` default is `md`
  - Rendering needs UI; compare tile with and without `size: md`
- [ ] **man-dashboard-41** (doc line 79) `"%.1f%%"` format works
  - Human: counter with `format: "%.1f%%"` and a value 12.34; expect `12.3%`
- [ ] **man-dashboard-45** (doc line 81) Table rows are whitespace-split from cron stdout
  - Human: create such a cron, table tile with `columns: [a, b]`, view
- [ ] **man-dashboard-46** (doc line 85) `warn`: value at or above renders yellow
  - Human: counter with value == warn; expect yellow. Existing example: prod.yaml counter warn 50 on `error-rate` (current value under `weft::latest error-rate` not read)
- [ ] **man-dashboard-47** (doc line 86) `crit`: value at or above renders red
  - Human: value == crit; expect red
- [ ] **man-dashboard-49** (doc line 87) `max` default is 100
  - Human: gauge without `max`, value 50 -> half bar

### Write, account or human

- [ ] **man-dashboard-11** (doc line 31-32) `vi /etc/dashboards/<name>.yaml` creates one
  - Human: `vi /etc/dashboards/audit.yaml`, paste YAML, save, then `dashboard audit` and `ls /etc/dashboards`. (Context only, prior dashboards.md audit: `vi` absent non-interactively.)
- [ ] **man-dashboard-12** (doc line 31-32) Filename without `.yaml` is the dashboard name
  - Human: save as `/etc/dashboards/x.yaml`, confirm `dashboard x` opens and `ls` shows `x.yaml`
- [ ] **man-dashboard-13** (doc line 36-58) The sample YAML is valid and loads
  - Needs write. Also sample refs crons `error-rate` (exists), `pending-jobs`, `top-errors`, `disk-pct` (not present: `ls /cron.d` -> `error-rate`, `mem-pressure` only), so the verbatim sample would be rejected here unless the reader creates those crons. Prior audit (dashboards.md dash-07) saw `unknown watches` rejection (not verified by me)
- [ ] **man-dashboard-14** (doc line 37) Top-level `title: Production`
  - See 13
- [ ] **man-dashboard-15** (doc line 38-52) `sections:` list of groups with `title` and `tiles`
  - See 13
- [ ] **man-dashboard-20** (doc line 46) `threshold: { warn: 100, crit: 500 }` flow-style accepted
  - Needs write+view. Note prod.yaml uses block-style `threshold:` maps (`warn: 50`/`crit: 200`), flow style not exercised by me
- [ ] **man-dashboard-26** (doc line 60-61) Flat `tiles:` list is shorthand for one untitled section
  - Human: save a file with only `tiles:` and run `dashboard <name>`
- [ ] **man-dashboard-27** (doc line 61) Mixing `sections:` and top-level `tiles:` is not allowed
  - Human: save YAML with both `sections:` and `tiles:`; prior audit (not mine) saw `cannot mix top-level tiles: with sections:`
- [ ] **man-dashboard-28** (doc line 65) `title` top-level optional heading
  - Human: omit `title:` and view
- [ ] **man-dashboard-29** (doc line 66) `sections` each with optional `title` and a `tiles` list
  - See 15
- [ ] **man-dashboard-30** (doc line 67) `tiles` top-level shorthand
  - See 26
- [ ] **man-dashboard-32** (doc line 72) Section `tiles` is required
  - Human: section with no `tiles:`; prior audit saw `section 0 ("A") has no tiles` with `tiles: []`
- [ ] **man-dashboard-33** (doc line 76) `kind` one of `sparkline`, `counter`, `gauge`, `table`
  - Human: `kind: pie`; prior audit (not mine) saw `unknown kind ... (valid: sparkline, counter, gauge, table)`
- [ ] **man-dashboard-35** (doc line 77) `watch` must exist when saved
  - Human: save with `watch: nonexistent`; expect save-time error (prior audit saw `dashboard references unknown watches`). Doc says only 'must exist when saved' but does not say what the error looks like
- [ ] **man-dashboard-37** (doc line 78) `size` is `sm`, `md`, or `lg`
  - Human: `size: xl`; prior audit saw `unknown size "xl" (valid: sm, md, lg)`
- [ ] **man-dashboard-40** (doc line 79) `format` is counter-only, printf-style
  - Human: `format` on a gauge; prior audit saw it accepted silently (doc says counter only; ambiguous whether error or ignore)
- [ ] **man-dashboard-42** (doc line 80) `threshold` applies to counter, gauge, sparkline
  - Human: `threshold` on a table; prior audit saw it accepted (not rejected)
- [ ] **man-dashboard-44** (doc line 81) `columns` is table-only
  - Human: `columns` on a counter; observe error vs ignored
- [ ] **man-dashboard-48** (doc line 87) `max` is gauge-only upper bound of bar
  - Human: `max` on a counter
- [ ] **man-dashboard-51** (doc line 93) `vi /etc/dashboards/ops.yaml` edits it
  - See 11
- [ ] **man-dashboard-55** (doc line 31) Editing /etc/dashboards requires configmgr (per team.md)
  - Needs write to /etc (denied by design under sh -c). `grep alice /etc/passwd` -> alice listed with `binmgr cronmgr configmgr nsmgr`, so the group exists; the doc itself (man-dashboard.md) never mentions configmgr, so a reader isn't told who may write /etc/dashboards
- [ ] **man-dashboard-56** (doc line 36-58) Sample sparkline tile has no `threshold` though allowed; sparkline renders thresholds
  - Human: add `threshold` to a sparkline tile; prior audit saw it accepted. Sample omission is harmless

## man-less

Source: `docs/audit/man-less.md`

### Terminal or browser

- [ ] **less-06** (doc line 26-27) Works the same in web shell as native CLI
  - web shell needs browser; CLI non-interactive `\| less` is a passthrough noop (see less-03)
- [ ] **less-08** (doc line 29-30) Result loaded into pager; shell enters ModeLess until q
  - needs interactive terminal to see ModeLess / q
- [ ] **less-10** (doc line 32-34) Help and man pages auto-page when output would overflow viewport
  - `help`/`man` are not available in `sh -c` (`bash: help: command not found`); auto-paging needs interactive terminal
- [ ] **less-11** (doc line 38) space, Page Down, f, Ctrl+F go forward one screen
  - key handling in pager requires interactive terminal/browser
- [ ] **less-12** (doc line 39) b, Page Up, Ctrl+B back one screen
  - key handling in pager requires interactive terminal/browser
- [ ] **less-13** (doc line 40) j, Down forward one line
  - key handling in pager requires interactive terminal/browser
- [ ] **less-14** (doc line 41) k, Up back one line
  - key handling in pager requires interactive terminal/browser
- [ ] **less-15** (doc line 42) g, gg jump to top
  - key handling in pager requires interactive terminal/browser
- [ ] **less-16** (doc line 43) G jump to bottom
  - key handling in pager requires interactive terminal/browser
- [ ] **less-17** (doc line 44) q, Ctrl+C exit the pager
  - key handling in pager requires interactive terminal/browser
- [ ] **less-18** (doc line 50) `/pattern` searches forward
  - key handling in pager requires interactive terminal/browser
- [ ] **less-19** (doc line 50) Search is case-sensitive substring (not regex)
  - key handling in pager requires interactive terminal/browser
- [ ] **less-20** (doc line 51) `n` next match
  - key handling in pager requires interactive terminal/browser
- [ ] **less-21** (doc line 52) `N` previous match
  - key handling in pager requires interactive terminal/browser
- [ ] **less-22** (doc line 57-58) Bottom row shows source title on left
  - key handling in pager requires interactive terminal/browser
- [ ] **less-23** (doc line 58) Percent position on right
  - key handling in pager requires interactive terminal/browser
- [ ] **less-24** (doc line 58-59) Incremental search and error messages replace status briefly
  - key handling in pager requires interactive terminal/browser

## man-passwd

Source: `docs/audit/man-passwd.md`

### Terminal or browser

- [ ] **man-passwd-16** (doc line 42) Or from dashboard's Members tab
  - Open dashboard, Members tab, confirm invite/remove controls.

### Write, account or human

- [ ] **man-passwd-05** (doc line 22) `adduser <email> [<group>]` syntax
  - Human: `adduser x@y.com` then `adduser x@y.com payments`. (Not run, forbidden.)
- [ ] **man-passwd-06** (doc line 23) `deluser <email> [<group>]` syntax
  - Human: `deluser x@y.com payments`, then `deluser x@y.com`.
- [ ] **man-passwd-15** (doc line 41-42) Mutating team via `adduser`/`deluser` is owner-only
  - Human: sign in as a plain member and run `adduser z@y.com`; expect denial. Note: the doc says owner-only but dev users have role `admin`; no `owner` role observed.
- [ ] **man-passwd-19** (doc line 49-50) `binmgr` grants edit of `/bin`
  - Needs a write to /bin by a binmgr vs non-binmgr member; human: as member without binmgr try `echo x > /bin/t.sh` (expect denial), then adduser grant. Note sh -c is zero-write by design.
- [ ] **man-passwd-20** (doc line 49-50) `cronmgr` grants edit of `/cron.d`
  - Same as 19 for /cron.d and cronmgr.
- [ ] **man-passwd-21** (doc line 49-50) `configmgr` grants edit of `/etc`
  - Same as 19 for /etc and configmgr.
- [ ] **man-passwd-22** (doc line 51) `<namespace>` group gives read (query) of `/log/<namespace>`
  - No non-admin member; human: grant `adduser u@x.com <ns>`, sign in as u, `tail /log/<ns>/...` works, other ns denied.
- [ ] **man-passwd-23** (doc line 52) `<namespace>+tokens` mints producer tokens for namespace
  - Human: grant `<ns>+tokens` then as that member `weft token create <ns>...`.
- [ ] **man-passwd-24** (doc line 53) +tokens independent of read; one, other, or both
  - Human: grant only +tokens, confirm token mint works but log read is denied.
- [ ] **man-passwd-26** (doc line 56) `+` is illegal in a namespace name
  - Human: `weft namespace create "a+b"` expect rejection.
- [ ] **man-passwd-27** (doc line 60) `adduser <email>` invites; sends invite email
  - Human: `adduser dana@acme.com`, check inbox.
- [ ] **man-passwd-28** (doc line 61) Invitee joins when they accept (or next sign in)
  - Needs a human accepting the invite email.
- [ ] **man-passwd-29** (doc line 62) `adduser <email> <group>` adds member to group
  - Human: `adduser bob@acme.com payments` then `groups bob@acme.com`.
- [ ] **man-passwd-30** (doc line 62-64) If email not on team yet, invited now and email goes out
  - Human: `adduser new@x.com payments`, check output says invited.
- [ ] **man-passwd-31** (doc line 64-65) Grant doesn't apply until accept; re-run afterwards
  - Human: after step 30, `groups new@x.com` shows no payments; accept invite; rerun adduser; groups shows payments. Read-side: invited row (stray alice) shows `groups=—` via `id alice` -> `uid=alice role=member groups=—`, consistent with no groups.
- [ ] **man-passwd-32** (doc line 65-66) Invited-but-not-accepted gives a clear error rather than silent no-op
  - Human: `adduser <invited-email> payments` and capture error text. Not run. Note the stray `alice` invited row exists and could be used.
- [ ] **man-passwd-33** (doc line 67) `deluser <email> <group>` revokes group
  - Human: `deluser bob@acme.com payments`, `groups bob@acme.com`.
- [ ] **man-passwd-34** (doc line 68) `deluser <email>` removes member entirely
  - Human: `deluser x@y.com`; `cat /etc/passwd` row gone.
- [ ] **man-passwd-37** (doc line 72) adduser/deluser owner-only
  - See 15.
- [ ] **man-passwd-38** (doc line 72-73) Group grants only after accept; no staging
  - See 31.
- [ ] **man-passwd-39** (doc line 78) `adduser dana@acme.com` # invite
  - Mutation + email; forbidden.
- [ ] **man-passwd-40** (doc line 79) `adduser dana@acme.com payments` lets dana read payments logs
  - See 29.
- [ ] **man-passwd-41** (doc line 80) `payments+tokens` lets dana mint producer tokens
  - Human: grant payments+tokens then mint token as dana.
- [ ] **man-passwd-42** (doc line 81) `cronmgr` lets dana edit /cron.d
  - Human: grant cronmgr then dana edits /cron.d.
- [ ] **man-passwd-43** (doc line 82) `deluser dana@acme.com payments` revokes read
  - See 33.
- [ ] **man-passwd-44** (doc line 83) `deluser dana@acme.com` removes from org
  - See 34.

## man-range

Source: `docs/audit/man-range.md`

### Terminal or browser

- [ ] **range-01** (doc line 7-8) Topics `man range` and `range --help` show this page
  - Human: type `man range` and `range --help` in interactive `weft sh`.
- [ ] **range-02** (doc line 9) Banner `RANGE(1)`
  - Human: `man range`, confirm first line RANGE(1).
- [ ] **range-07** (doc line 28-29) Setting range applies to all subsequent queries
  - Human: in interactive `weft sh` run `range 1m..` then `tail <file>` and check timestamps are within the window.
- [ ] **range-09** (doc line 30-32) Relative specs re-resolved against server clock on every query
  - Human: `range 1m..`, run a query, wait 2 min, run again; confirm window moved.
- [ ] **range-27** (doc line 60-61) Re-anchored on every query so long-lived pane never stale
  - Human: interactive `weft sh`, `range 1m..`, wait, query twice.

## metavars

Source: `docs/audit/metavars.md`

### Terminal or browser

- [ ] **metavars-59** (doc line 3-7) front matter: id manual-metavars, section metavars, category query
  - web docs sidebar check requires browser

### Write, account or human

- [ ] **metavars-11** (doc line 30) `VERSION` / `._version` from weftd `version.file:` (live) or `version.env:` (snapshot) | needs-human | requires weftd configured on a host
  - needs weftd/cloud/k8s config; dev stack rows show this column as empty string (`._region` -> "")
- [ ] **metavars-12** (doc line 31) `REGION` / `._region` from cloud metadata probe at agent startup | needs-human | requires weftd on cloud instance
  - needs weftd/cloud/k8s config; dev stack rows show this column as empty string (`._region` -> "")
- [ ] **metavars-13** (doc line 32) `CLUSTER` / `._cluster` from weftd k8s mode | needs-human | requires k8s cluster
  - needs weftd/cloud/k8s config; dev stack rows show this column as empty string (`._region` -> "")
- [ ] **metavars-14** (doc line 33) `K8S_NAMESPACE` / `._k8s_namespace` from weftd k8s mode, parsed from /var/log/pods/... | needs-human | k8s
  - needs weftd/cloud/k8s config; dev stack rows show this column as empty string (`._region` -> "")
- [ ] **metavars-15** (doc line 34) `POD` / `._pod` k8s mode | needs-human | k8s
  - needs weftd/cloud/k8s config; dev stack rows show this column as empty string (`._region` -> "")
- [ ] **metavars-16** (doc line 35) `CONTAINER` / `._container` k8s mode | needs-human | k8s
  - needs weftd/cloud/k8s config; dev stack rows show this column as empty string (`._region` -> "")
- [ ] **metavars-17** (doc line 36) `NODE` / `._node` from weftd `dimensions.node:` (file-sourced) | needs-human | weftd config
  - needs weftd/cloud/k8s config; dev stack rows show this column as empty string (`._region` -> "")
- [ ] **metavars-58** (doc line 169-170) REGION populated from cloud metadata at agent startup; rollup gives level-by-region histogram | needs-human | cloud instance with weftd
  - needs weftd/cloud/k8s config; dev stack rows show this column as empty string (`._region` -> "")

## namespaces

Source: `docs/audit/namespaces.md`

### Write, account or human

- [ ] **namespaces-01** (doc line 12-13) Namespaces group related log sources
  - Human: `weft namespace create demo` then `weft sh -c 'ls /log'`. Read-only: `weft namespace list` shows `acme`, `audit-metavars-ns`; `ls /log` shows `acme/`.
- [ ] **namespaces-05** (doc line 15-16) Create flow accepts a short lowercase name
  - Human: create `demo` via UI form. Static: no lowercase/a-z handling found in Namespaces.tsx.
- [ ] **namespaces-06** (doc line 16) Uppercase names are not allowed / name should be lowercase
  - Human: `weft namespace create Demo`. Read-only note: `ls /log/Acme` -> `ls: file does not exist` (paths are case-sensitive, not folded).
- [ ] **namespaces-11** (doc line 20) `namespace create` output/effect: namespace appears in `/log` and Namespaces page
  - Needs create. Read-only: `ls /log` lists `acme/`, `weft namespace list` lists acme.
- [ ] **namespaces-24** (doc line 46-47) `main` is default kind a producer ships unless it says otherwise
  - Needs shipping a line with a producer. All existing streams ship `main.log`; no direct test for 'default'.
- [ ] **namespaces-27** (doc line 52) Each producer decides service name; weft never rewrites it
  - Human: mint token, ship with service `My_Svc`, `ls /log/<ns>/<env>`.
- [ ] **namespaces-28** (doc line 56) weftd: service = source's `service:` setting
  - Human: configure source with `service:` and check path.
- [ ] **namespaces-29** (doc line 56) weftd: if omitted, systemd `unit:` name without `.service`
  - Human: source with `unit: foo.service`, no service; expect `foo`.
- [ ] **namespaces-30** (doc line 56) weftd: else log file's name without its extension
  - Human: file source `/var/log/x.log` with no service; expect `x`.
- [ ] **namespaces-31** (doc line 56) weftd kind is `main` unless source sets a kind
  - Human: check path is `.../main.log` without kind set.
- [ ] **namespaces-32** (doc line 57) Heroku: service = Heroku app name
  - Needs Heroku account.
- [ ] **namespaces-33** (doc line 57) Heroku: kind = dyno process type (`web`, `worker`, ...)
  - Needs Heroku account.
- [ ] **namespaces-34** (doc line 57) Heroku: platform events go to kind `platform`
  - Needs Heroku account.
- [ ] **namespaces-35** (doc line 58) Render: service = Render service slug
  - Needs Render account.
- [ ] **namespaces-36** (doc line 58) Render: kind = process type (`web`, `cron`, `bld`, ...)
  - Needs Render account.
- [ ] **namespaces-38** (doc line 59) CloudWatch: events from unregistered log groups are dropped
  - Needs AWS account.
- [ ] **namespaces-39** (doc line 59) CloudWatch kind is `main`
  - Needs AWS account.
- [ ] **namespaces-40** (doc line 61-62) A namespace can hold env-less and env-bearing streams side by side as tokens reauth
  - Needs minting env-less and env tokens in one namespace. 'as different tokens reauth over time' is also unclear wording. Currently acme has only env-bearing layout.

## notify

Source: `docs/audit/notify.md`

### Terminal or browser

- [ ] **notify-10** (doc line 28) Edit with `vi /etc/notify.yaml`
  - `weft sh` then `vi /etc/notify.yaml` (non-interactive `sh -c` cannot run an editor; write is also 🔒). Static: help alerts also says "vi /etc/notify.yaml".
- [ ] **notify-14** (doc line 35-36) Same functionality under **Account -> Integrations** in browser
  - Open the web UI, Account menu, look for Integrations page.
- [ ] **notify-15** (doc line 36) CLI and browser surfaces do the same thing
  - Add a PagerDuty name with CLI (🔒 write), reload Account -> Integrations in browser and confirm it appears.

### Write, account or human

- [ ] **notify-06** (doc line 19-25) YAML shape: alias -> {slack: [..], pagerduty: [..], email: [..]} accepted
  - Needs write to /etc. Step: `weft sh`, `vi /etc/notify.yaml`, paste the doc block (needs configmgr), save, expect no validation error. Read-only: live file parses with same shape (alias -> webhook/email lists), no slack/pagerduty example present.
- [ ] **notify-16** (doc line 39-41) `weft integration pagerduty add ops-daytime --key R0UTING_KEY_FROM_PD` registers name with key
  - Writes account state. Step: `weft integration pagerduty add ops-daytime --key <real key>` then `weft integration list`. Read-only: `weft integration pagerduty` -> "usage: weft integration pagerduty {add|rm} ..."; list hint shows `weft integration pagerduty add <name> --key <key>` (matches doc form).
- [ ] **notify-17** (doc line 40) The name is what notify.yaml references under `pagerduty:`
  - Step: add name then reference it under `pagerduty:` and fire; static help alerts says name must match.
- [ ] **notify-18** (doc line 42) `weft integration pagerduty rm ops-daytime` removes it
  - Writes account state. Step: `weft integration pagerduty rm ops-daytime` then `weft integration list`. Subcommand `rm` confirmed in usage output.
- [ ] **notify-19** (doc line 44-46) Slack uses OAuth; connect opens a browser tab (like `weft login`)
  - Slack OAuth; step: `weft integration slack connect`, browser tab opens, grant access.
- [ ] **notify-20** (doc line 46) CLI waits and confirms once workspace connected
  - Step: run connect, confirm CLI waits then prints confirmation.
- [ ] **notify-24** (doc line 54-55) PagerDuty alias in notify.yaml must match a registered name
  - Step: put unregistered pagerduty name in notify.yaml, fire alert, observe error. Static help alerts agrees.
- [ ] **notify-25** (doc line 55-57) Slack channels named directly in notify.yaml once workspace connected
  - Needs a connected Slack workspace.
- [ ] **notify-28** (doc line 65-71) YAML shape: alias `pages` with webhook and email lists is valid
  - Needs write to /etc. Step: `vi /etc/notify.yaml`, add `pages:` block, save. Read-only: same shape (webhook+email lists) already in live file under `ops`.
- [ ] **notify-30** (doc line 77-79) `echo "test fire" \| alert --notify=ops` tests an alias
  - Forbidden this run (would POST to third-party webhook/email). Note: live alias `ops` points at webhook https://hooks.loom.test/weft/ops and emails, so the doc example hits real destinations. Step: `echo "test fire" | alert --notify=ops` in `weft sh`.

## query-logs

Source: `docs/audit/query-logs.md`

### Terminal or browser

- [ ] **query-logs-01** (doc line 12) Web UI has a "Shell" and there is a `weft` CLI
  - needs web UI; CLI `weft --help` lists `sh`, `sh -c` (ok)

### Write, account or human

- [ ] **query-logs-22** (doc line 50) HOST/INSTANCE = agent's hostname
  - hostnames are container ids; needs weftd host
- [ ] **query-logs-25** (doc line 53) `DEPLOYMENT`/`._deployment` = weftd config `deployment:` value
  - needs weftd/k8s config; jq on metrics shows these keys exist (empty string) e.g. `_cluster:""`,`_pod:""`,`_region:""`,`_version:""`; DEPLOYMENT=api observed
- [ ] **query-logs-28** (doc line 55) `VERSION`/`._version` = per-source `version_file` watched by the agent
  - needs weftd/k8s config; jq on metrics shows these keys exist (empty string) e.g. `_cluster:""`,`_pod:""`,`_region:""`,`_version:""`; DEPLOYMENT=api observed
- [ ] **query-logs-29** (doc line 56) `REGION`/`._region` detected from cloud metadata at agent startup
  - needs weftd/k8s config; jq on metrics shows these keys exist (empty string) e.g. `_cluster:""`,`_pod:""`,`_region:""`,`_version:""`; DEPLOYMENT=api observed
- [ ] **query-logs-30** (doc line 57) `CLUSTER`/`._cluster` k8s mode only, from DaemonSet config
  - needs weftd/k8s config; jq on metrics shows these keys exist (empty string) e.g. `_cluster:""`,`_pod:""`,`_region:""`,`_version:""`; DEPLOYMENT=api observed
- [ ] **query-logs-31** (doc line 58) `K8S_NAMESPACE`/`._k8s_namespace` k8s mode only
  - needs weftd/k8s config; jq on metrics shows these keys exist (empty string) e.g. `_cluster:""`,`_pod:""`,`_region:""`,`_version:""`; DEPLOYMENT=api observed
- [ ] **query-logs-32** (doc line 59) `POD`/`._pod` k8s mode only
  - needs weftd/k8s config; jq on metrics shows these keys exist (empty string) e.g. `_cluster:""`,`_pod:""`,`_region:""`,`_version:""`; DEPLOYMENT=api observed
- [ ] **query-logs-33** (doc line 60) `CONTAINER`/`._container` k8s mode only
  - needs weftd/k8s config; jq on metrics shows these keys exist (empty string) e.g. `_cluster:""`,`_pod:""`,`_region:""`,`_version:""`; DEPLOYMENT=api observed
- [ ] **query-logs-34** (doc line 61) `NODE`/`._node` = stable workload identity, file-sourced via agent `dimensions:` config, never auto-derived
  - needs weftd/k8s config; jq on metrics shows these keys exist (empty string) e.g. `_cluster:""`,`_pod:""`,`_region:""`,`_version:""`; DEPLOYMENT=api observed

## render

Source: `docs/audit/render.md`

### Terminal or browser

- [ ] **render-07** (doc line 28) Web UI: Namespaces -> your namespace -> Create token
  - Open web UI, Namespaces, pick a namespace, confirm a Create token control.
- [ ] **render-08** (doc line 28-29) UI has a **For** dropdown with a **Render** option and a **Create token** button
  - In Create token dialog verify label "For", option "Render", button "Create token".

### Write, account or human

- [ ] **render-01** (doc line 12-13) weft accepts logs from Render's built-in Log Streams feature
  - Render account needed: add a Log Stream in Render pointed at the endpoint and `tail` the pseudofile.
- [ ] **render-02** (doc line 13-14) No agent, no AWS account, no schema file required
  - Same as 01; no agent/AWS/schema step appears anywhere in the CLI token flow (static read of `weft token create` usage).
- [ ] **render-06** (doc line 25-26) Minting a token for Render reveals the endpoint hostname
  - Creating a token is disallowed here. Static: cmd/weft/main.go prints `Endpoint:` only when `--for` is given. Usage line `weft token create --namespace NAME [--label LABEL] [--env ENV] [--for PRODUCER]` observed.
- [ ] **render-16** (doc line 47-48) Render dashboard: service -> **Settings -> Log Streams** -> add stream -> generic/custom syslog destination
  - Render dashboard needed.
- [ ] **render-18** (doc line 51) Token field takes the `wft_...` token
  - Render dashboard needed.
- [ ] **render-20** (doc line 55-56) Weft's endpoint falls back to generic syslog: plain TLS syslog with token prepended as literal string before every line
  - Needs a real token sent over TLS to the (shared) syslog endpoint: write side effect on ingest. Human step: `printf "wft_TOKEN <14>1 2026-01-01T00:00:00Z h myservice web-9nk6q - - hello\n" | openssl s_client -quiet -connect render.syslog.w3ft.net:6514` then `tail` the file.
- [ ] **render-23** (doc line 60) Render starts streaming within a few seconds
  - Render account needed.
- [ ] **render-26** (doc line 72) Render service slug becomes weft service name
  - Needs Render-style syslog ingest with a producer token.
- [ ] **render-28** (doc line 73-74) Process types are `web`, `cron`, and an "undocumented" `bld` for build output
  - Needs ingest.
- [ ] **render-29** (doc line 74-75) Process type becomes `kind` (file name stem)
  - Needs ingest.
- [ ] **render-30** (doc line 75-76) Trailing instance id stamped on `instance` for per-instance filtering without splitting pseudofile
  - Needs ingest.
- [ ] **render-31** (doc line 80) `myservice` web instance `web-9nk6q` -> `/log/<ns>/myservice/web.log` with `instance=9nk6q`
  - Needs ingest; step: send frame with source `web-9nk6q`, then `tail file | jq .instance`, expect `9nk6q`.
- [ ] **render-32** (doc line 81) cron job `cron-stqq5` -> `/log/<ns>/myservice/cron.log`
  - Needs ingest.
- [ ] **render-33** (doc line 82) build output `bld-...` -> `/log/<ns>/myservice/bld.log`
  - Needs ingest.
- [ ] **render-36** (doc line 89) Ingest capped per token, not per connection
  - Needs live ingest and rate-limit test.
- [ ] **render-37** (doc line 89-90) Reconnecting doesn't reset the budget
  - Needs live ingest.
- [ ] **render-39** (doc line 91-92) Over-rate rows are delayed, never dropped, until budget refills
  - Needs live ingest.
- [ ] **render-41** (doc line 97-99) Failing status in Render dashboard usually means token/endpoint mistyped
  - Render dashboard needed.
- [ ] **render-43** (doc line 101-102) weft trusts Render's service slug as-is
  - Needs ingest.
- [ ] **render-44** (doc line 102-104) Fix for duplicate service names across Render teams: separate tokens in separate namespaces
  - Needs token creation in two namespaces + ingest.
- [ ] **render-45** (doc line 105-106) Build output streams only while a build is running; trigger deploy to see it
  - Render account needed.
- [ ] **render-46** (doc line 107-111) Vector can receive syslog and re-forward to weft's HTTP ingest endpoint
  - Vector setup needed. Doc never names the HTTP ingest endpoint.

## schema-setup

Source: `docs/audit/schema-setup.md`

### Terminal or browser

- [ ] **schema-02** (doc line 13) A **Namespaces** page exists in the web UI
  - Log in to the web UI at the dev endpoint and look for Namespaces in the nav.
- [ ] **schema-03** (doc line 13-14) Namespaces shows detected apps and their field schemas
  - After ingest, open Namespaces and check the app list shows each app with its field schema.
- [ ] **schema-04** (doc line 16-17) A **pending source** notification appears on the dashboard when data arrived but schema is unconfirmed
  - Needs a fresh weftd source; open the dashboard and look for the "pending source" notification.
- [ ] **schema-05** (doc line 17-18) Notification means weft received data but needs you to confirm detected schema
  - Click the pending source notification and read its text.
- [ ] **schema-06** (doc line 18-19) You can review sample lines and approve
  - Click the notification; confirm sample lines are shown and an approve control exists.
- [ ] **schema-14** (doc line 4-7) Frontmatter: section schema-setup, category connect-source, order 45
  - Static: frontmatter present (section schema-setup, category connect-source, order 45); generated frontend/src/generated/docs/schema-setup.ts and `man schema-setup` entry in shell/help_corpus_gen.go exist with no stray `{{`/`&lt;` in schema-setup.ts. Web manual placement needs a browser.

### Write, account or human

- [ ] **schema-01** (doc line 12-13) Once weftd ships logs, weft auto-detects schema from the first batch of lines
  - Needs weftd on a host: install weftd, point at a fresh log file, send a first batch, then open Namespaces. Not runnable read-only.

## scripts

Source: `docs/audit/scripts.md`

### Terminal or browser

- [ ] **scripts-04** (doc line 14) Create scripts with vim
  - Human: `vi /bin/t1`, type body, `:w`. Doc says "vim" (l.14) but the command is `vi` (l.112).
- [ ] **scripts-47** (doc line 112) `vi /bin/<name>` opens the script for editing
  - Human: `vi /bin/count_errors` in `weft sh`, confirm editor opens.
- [ ] **scripts-48** (doc line 112-113) After `:w` shell drops into follow-up buffer asking for optional commit message
  - Human: `vi /bin/t1`, edit, `:w`; confirm follow-up commit-message buffer appears.
- [ ] **scripts-49** (doc line 113-114) `:wq` records the message, `:q!` skips
  - Human: in the follow-up buffer type a message, `:wq`, then `git log -- /bin/t1`; repeat with `:q!`. Note: `git log /bin/t1` (without `--`) -> `fatal: bad revision '/bin/t1'`; needs `git log -- /bin/t1`.

### Write, account or human

- [ ] **scripts-01** (doc line 12) Anything typeable at the shell prompt can be saved as a named script under /bin/
  - Human: in `weft sh`, `echo "echo hi" > /bin/t1` then `t1`. Static: seeded /bin has count_errors, scan, severity, mem_history, status.lua (`ls /bin`), so scripts under /bin do exist and run.
- [ ] **scripts-02** (doc line 13) Scripts are available from every shell session
  - Human: save /bin/t1 in one session, open second `weft sh`, run `t1`. Partial: `./weft sh -c count_errors`/`scan` from separate invocations both run seeded /bin scripts (`scan` -> 100).
- [ ] **scripts-03** (doc line 13) Scripts are available to every cron in the account
  - Human: add cron body `t1`, then check result with `weft::latest <cron>`.
- [ ] **scripts-05** (doc line 14) Create scripts with `cat - > myscript`
  - Human: `cat - > /bin/t2`, type body, Ctrl-D, `ls /bin`. Note: `cat - > /tmp/x` redirect itself works (`echo a > /tmp/audit-scripts-x; cat /tmp/audit-scripts-x` -> `a`).
- [ ] **scripts-06** (doc line 15) Writing the file validates syntax errors
  - Human: `echo "if then" > /bin/bad` expect rejection; `ls /bin` no `bad`. (Same write to /tmp is accepted without validation, which is expected: no script validation outside /bin.)
- [ ] **scripts-07** (doc line 16) Writing validates "some weft-specific semantics"
  - Human: save a Lua script calling `weft.nope()` or a bad cron name and see whether write is rejected. Doc does not say which semantics are checked (ambiguous wording).
- [ ] **scripts-10** (doc line 24-25) /bin/count_errors body `cat /log/$1 \| grep ERROR \| wc -l` is valid and saves
  - Save is a /bin write. Body syntax is valid as a pipeline: `cat /log/acme/prod/api/main.log \| tail -300 \| grep ERROR \| wc -l` -> 300. Note seeded count_errors uses awk (pushdown) not grep.
- [ ] **scripts-15** (doc line 40) Scripts default to bash
  - Default-bash needs a saved script. Seeded `count_errors` and `scan` have no shebang and run as bash pipelines (`scan` -> 100).
- [ ] **scripts-35** (doc line 81) `arg` global holds positional args arg[1], arg[2]
  - Human: save /bin/a with `print(arg[1], arg[2])`, run `a x y`. Seeded count_errors shows $1 binding only for bash.
- [ ] **scripts-52** (doc line 118) Five most recent past versions are kept; older discarded
  - Human: save /bin/t1 8 times with messages; `git log -- /bin/t1 \| grep -c ^commit`. Ambiguity: "five most recent past versions" vs current, is it 5 or 6 commits?
- [ ] **scripts-53** (doc line 119-120) No recycle bin past the cap (older versions unrecoverable)
  - Needs the 8 saves above, then `git restore` of oldest. Human step: after saving 8 times, `git log -- /bin/t1` and try `git checkout <oldest-sha> -- /bin/t1`; expect failure.

## sharing

Source: `docs/audit/sharing.md`

### Terminal or browser

- [ ] **sharing-05** (doc line 22-25) Pasting URL: signed-in teammate who clicks lands in the web shell with script invocation pre-filled
  - Open the printed link in a browser signed in as bob (acme); expect web shell with `scan` pre-filled.
- [ ] **sharing-06** (doc line 24-25) Teammate hits Enter and sees same output
  - In the same browser, press Enter on the prefilled command; compare output to alice running `scan`.
- [ ] **sharing-07** (doc line 24) Link is not auto-executed (pre-filled only, requires Enter)
  - Open link; confirm prompt shows command but no output until Enter is pressed.
- [ ] **sharing-20** (doc line 50-51) Link carries only range + args, behind a short link
  - Link token is a 10-char id, no script body in URL (static from format). Verify in browser network tab: GET of the /s/<id> resolves via API; confirm the URL carries only the id.
- [ ] **sharing-23** (doc line 58) ctrl-d ends input (in both CLI and web shell)
  - In the web shell run `cat - > /bin/check-foo`, type text, press Ctrl-D; confirm input ends. Repeat in CLI.
- [ ] **sharing-33** (doc line 85) Link does not grant access
  - Open link in browser as globex user dana and logged out; expect no access. Doc claim "does not grant access" is policy, black-box needs browser.
- [ ] **sharing-34** (doc line 85-86) Link opens only for org members; others, signed in or not, get "that link doesn't exist"
  - Open link signed out, and signed in as dana (globex) or kai (loom); expect exact "that link doesn't exist". Static: string present in corpus (ManClipboard and ManualSharing).
- [ ] **sharing-35** (doc line 86-88) Recipient must be signed in to the same org
  - Open link in logged-out browser: observe whether login redirect or "that link doesn't exist". See sharing-38.
- [ ] **sharing-37** (doc line 88-90) Link leaks no log data
  - Open link signed out and inspect page source/network: expect no log data. Token is opaque 10 chars (no payload in URL, static).

### Write, account or human

- [ ] **sharing-15** (doc line 39) Recipient lands at script's HEAD version
  - Needs a persistent write: share link, `vi /bin/scan` (edit), reopen the link in the web shell and confirm new body runs. Not run (read-only rules).
- [ ] **sharing-22** (doc line 56-60) `cat - > /bin/check-foo` reads stdin until ctrl-d and writes script
  - `cat - > /bin/check-foo` writes to /bin (persistent). Human step: in interactive `weft`, type `cat - > /bin/check-foo`, paste a pipeline, Ctrl-D, then `cat /bin/check-foo`. (Requires binmgr group.)
- [ ] **sharing-24** (doc line 59) `clipboard /bin/check-foo` then works
  - Depends on sharing-22 having created /bin/check-foo. Human step: after creating, `clipboard /bin/check-foo`; expect "copied: https://w3ft.net/s/<id>". (Proxy: `clipboard /bin/scan` works for existing scripts.)
- [ ] **sharing-36** (doc line 87-88) Recipient needs namespace permissions to run script
  - No non-admin user in acme (all admin/owners with implicit full access); permission-difference not testable. Human step: invite a member without the namespace group, open link, run prefilled script, expect denial.

## team

Source: `docs/audit/team.md`

### Terminal or browser

- [ ] **team-09** (doc line 16) Doc shows `$ ` prompt for the shell
  - `sh -c` prints no prompt; open interactive `weft sh` and compare prompt to `$ `
- [ ] **team-12** (doc line 24) Team can also be managed from dashboard's Members tab
  - Open web dashboard, look for a Members tab
- [ ] **team-46** (doc line 85) `man passwd` in shell is quick reference
  - `man` unavailable under sh -c; type `man passwd` in interactive shell. Static: docs/manual/man-passwd.md does not exist at that path
- [ ] **team-47** (doc line 8) Doc placement: Getting Started order 30
  - Open web docs sidebar and check Team & permissions order under Getting Started

### Write, account or human

- [ ] **team-10** (doc line 23-24) You never edit the file; team managed via `adduser`/`deluser`
  - `man` unavailable under sh -c; 🖥 type `man adduser`. adduser/deluser forbidden
- [ ] **team-11** (doc line 23-24) `adduser` and `deluser` are owner-only
  - No non-admin user in acme; as non-admin member run `adduser x@y.com`, expect permission error
- [ ] **team-13** (doc line 24-25) Shell commands and Members tab write the same underlying data
  - Run `adduser x@y.com` in shell, refresh Members tab (needs write + browser), then reverse
- [ ] **team-16** (doc line 35) `binmgr` grants edit of `/bin` (functions)
  - Needs write to /bin; step: as member with binmgr run `sh -c "echo x > /bin/t"`; without, expect denied
- [ ] **team-17** (doc line 36) `cronmgr` grants edit of `/cron.d` (crons)
  - Needs write to /cron.d; step: as cronmgr member create a cron; without, expect denied
- [ ] **team-18** (doc line 37) `configmgr` grants edit of `/etc` (notify.yaml, dashboards)
  - Needs write to /etc; step: as configmgr member edit /etc/notify.yaml; without, denied
- [ ] **team-23** (doc line 39-40) Namespace groups gate log access, one per namespace
  - Needs adduser; step: `adduser <email> <ns>` per namespace, then `cat /etc/passwd`. Read-side: acme roster lists group `acme` per namespace
- [ ] **team-24** (doc line 43) Group `<namespace>` grants read (query) of `/log/<namespace>`
  - Needs a non-admin member without the group; step: as that member `cat /log/acme/prod/api/main.log | head -1`, expect denied
- [ ] **team-25** (doc line 44) Group `<namespace>+tokens` grants minting producer tokens for that namespace
  - Needs non-admin member; step: `weft token create` as member with and without acme+tokens
- [ ] **team-26** (doc line 46) Read and produce are independent; member can have either or both
  - Needs adduser to grant only `<ns>+tokens`; then confirm read denied. Read-side: carol has `acme` without `acme+tokens`, so the facets are stored independently
- [ ] **team-27** (doc line 47-48) `+tokens` facet uses `+` not `:` so it cannot collide with a namespace name
  - Namespace creation forbidden; step: `namespace create "a+tokens"` expect rejection
- [ ] **team-30** (doc line 55) `adduser dana@acme.com` invites and sends an invite email
  - Sends email; needs adduser and a real inbox
- [ ] **team-31** (doc line 56) `deluser dana@acme.com` removes from the org entirely
  - Needs deluser; step: `deluser x@y.com; cat /etc/passwd`
- [ ] **team-32** (doc line 59-60) `adduser <email>` with no group sends an invite link
  - Needs adduser; inspect invite email
- [ ] **team-33** (doc line 60) Person joins when they accept, or next time they sign in with that email
  - Needs second email account sign-in
- [ ] **team-34** (doc line 67) `adduser dana@acme.com payments` grants read of payments logs
  - Needs adduser plus accepted member; Dana then queries /log/payments
- [ ] **team-35** (doc line 68) `adduser ... payments+tokens` grants producer token minting
  - Needs adduser, then Dana mints token
- [ ] **team-36** (doc line 69) `adduser ... cronmgr` grants editing `/cron.d`
  - Needs adduser; see 17
- [ ] **team-37** (doc line 70) `deluser dana@acme.com payments` revokes read
  - Needs deluser; Dana queries after revoke
- [ ] **team-40** (doc line 76-77) Group grants apply only once member has accepted invite; no staging
  - Needs adduser on pending member
- [ ] **team-41** (doc line 77-79) `adduser` to a group for non-member invites them then and there (email goes out)
  - Needs adduser and inbox
- [ ] **team-42** (doc line 79) After they accept you must re-run the command
  - Needs adduser and accept flow
- [ ] **team-43** (doc line 79-80) Already-invited, not-yet-accepted member gets a clear error not silent no-op
  - Needs adduser on invited member; capture error
- [ ] **team-44** (doc line 82-83) `groups` and `id` are readable by any member
  - No non-admin acme user; groups/id verified for an admin only. Step: run `groups` and `id` as plain member
- [ ] **team-45** (doc line 82) `adduser`/`deluser` owner-only (restated)
  - See 11

## tmp

Source: `docs/audit/tmp.md`

### Terminal or browser

- [ ] **tmp-09** (doc line 32) `vi /tmp/incident-2026-04-22/notes`
  - `vi` not available in sh -c. Also depends on mkdir (fails). Human step: `weft sh`, then `vi /tmp/notes`, type, save.
- [ ] **tmp-37** (doc line 93-94) Scripts live in /bin; see **Custom scripts**
  - Open the web manual page for Custom scripts and confirm the section name matches; `ls /bin` does list scripts (count_errors, scan, ...). Cross-ref is plain bold text, not a link.

### Write, account or human

- [ ] **tmp-19** (doc line 50) Files expire after 48 hours
  - Needs 48h wait. Human step: `echo x > /tmp/probe` today; after 48h+ run `ls /tmp` and confirm gone. No expiry indicator is visible in `ls -l` or `cat /etc/usage`.
- [ ] **tmp-20** (doc line 52) "Despite the shell rules" /tmp is durable: files survive logouts
  - Needs re-login. Human step: write `/tmp/probe`, exit `weft sh`, start new `weft sh`, `ls /tmp`. Note: separate `sh -c` invocations (separate sessions) did see each other's files (files written in one call read in the next), which is partial evidence of persistence across sessions. "Despite the shell rules" phrase unclear (style).
- [ ] **tmp-22** (doc line 53-54) Every file deleted 48h after last edited
  - Same as tmp-19; needs 48h.
- [ ] **tmp-27** (doc line 64-67) Long-term storage: save pipeline as cron (see **Crons** section); cron results retained for full plan history
  - Needs web UI Crons manual check and retention is plan dependent. Step: open manual Crons section, confirm heading exists and describes result retention. `ls -l /cron.d` works and lists crons.
- [ ] **tmp-28** (doc line 71-76) Plan tiers: Trial, Swatch, Yard, Bolt
  - Needs web UI billing/plans page. Observed: `whoami` says `Plan: yard`, but `cat /etc/usage` says `tier: trial` for the same org (inconsistent). Human step: open Settings > Billing and compare names Trial/Swatch/Yard/Bolt.
- [ ] **tmp-29** (doc line 73) Trial: 500 KB total, 512 KB per file
  - Needs a Trial org to test. Note: 500 KB total < 512 KB per-file, so per-file limit is unreachable on Trial (doc inconsistency). Acme shows plan yard.
- [ ] **tmp-30** (doc line 74) Swatch: 10 MB, 512 KB per file
  - Plan-specific; needs Swatch org. Human step: write files until 10 MB total and check the error.
- [ ] **tmp-31** (doc line 75) Yard: 100 MB, 512 KB/file
  - Plan-specific (acme is `Plan: yard` per whoami). Total 100 MB not testable without writing 100 MB of 512 KB files (~200 writes). Per-file part verified in tmp-33.
- [ ] **tmp-32** (doc line 76) Bolt: 1 GB, 512 KB/file
  - Plan-specific; needs Bolt org.
- [ ] **tmp-41** (doc line 99-101) /tmp per-customer; every member of account sees same /tmp; other accounts see none | needs-human | requires two users/two accounts
  - Needs a second user and another org. Step: as bob (acme) `ls /tmp` sees alice's file; as dana (globex) `ls /tmp` sees none. Not run to avoid scope creep; /tmp listing showed only `Alice Chen` as owner so far.
- [ ] **tmp-49** (doc line 121-122) `cat /tmp/x > /bin/y` works via normal save path (with validation)
  - `cat /tmp/x > /bin/y` -> permission denied in this read-only environment (expected). Human step with binmgr: `echo 'echo hi' > /tmp/s; cat /tmp/s > /bin/s` then try invalid content and confirm a validation error.

## tokens

Source: `docs/audit/tokens.md`

### Terminal or browser

- [ ] **tok-03** (doc line 13) Web UI has **Namespaces** page; select a namespace
  - Log in to the web UI, open Namespaces, pick a namespace; confirm the page lists it.
- [ ] **tok-04** (doc line 13) A **Create token** button exists on the namespace page
  - On the namespace page look for a Create token button and click it.
- [ ] **tok-11** (doc line 31) Token-create form has an Env field
  - Open Create token form in web UI; confirm an Env field. Static: generated tokens.ts contains the sentence but that is only the doc itself.
- [ ] **tok-25** (doc line 2-7) Frontmatter: section tokens, category getting-started, order 20
  - Open the web manual; confirm Tokens appears in Getting started at position order 20.

### Write, account or human

- [ ] **tok-01** (doc line 12) Tokens authenticate weftd agents
  - Needs a host running weftd with a good and a bad token. Step: set `token:` in weftd config, start weftd, confirm lines appear in /log/<ns>/...; repeat with a wrong token and expect rejection. (Active ingest tokens exist in `weft token list`, e.g. acme-api-prod, but no agent was run.)
- [ ] **tok-07** (doc line 18-21) weftd config key is `token: wft_...`
  - Needs a host with weftd. Step: add `token: wft_...` to weftd config, restart weftd, confirm connection. Note `weft token create` output also says 'Store this token in your weftd config'.
- [ ] **tok-09** (doc line 26-27) Every row from an agent using that token has env stamped before reaching a shard
  - Needs an agent shipping lines with an env-bound token. Step: ship a line with the acme-api-prod-style token, then `tail -n 3 /log/acme/prod/api/main.log / awk '{print ENV}'` (that command printed `prod` for existing rows, but that does not prove stamping at ingest).
- [ ] **tok-10** (doc line 27-29) Agent cannot override env; leaked prod token can't write rows tagged staging
  - Needs an agent claiming ENV=staging with a prod token; confirm the row still reads ENV=prod.
- [ ] **tok-16** (doc line 37) Every row picks up bound env at ingest time
  - Same as tok-09: stamping at ingest needs an agent.
- [ ] **tok-17** (doc line 37-38) Any query the token authenticates implicitly filters to that env
  - Needs a query authenticated by an env-bound token. Step: set WEFT_TOKEN to a token with env bound and run `tail -n 5 /log/<ns>/...`. Doc is also ambiguous: doc says tokens authenticate weftd agents (ingest), then says 'any query the token authenticates' implying query use; unclear whether ingest tokens can query.

## weft-funcs

Source: `docs/audit/weft-funcs.md`

### Terminal or browser

- [ ] **funcs-38** (doc line 93-94) Same behavior as the `alert` builtin on those conditions
  - `man alert` and `alert` not available in `weft sh -c` (command not found)
- [ ] **funcs-60** (doc line 148) `man alert` exists
  - `man alert` -> `bash: man: command not found` in `weft sh -c`; help/man are interactive-only. docs/shell/man-alert.md exists

## weftd

Source: `docs/audit/weftd.md`

### Terminal or browser

- [ ] **weftd-04** (doc line 13) weftd is downloadable from the Downloads page
  - Open web UI Downloads page, confirm a weftd entry is listed
- [ ] **weftd-35** (doc line 91-93) **Meta-variables** section in **Query your logs** doc lists them and shows awk/jq use
  - Web UI: Docs > Query your logs, confirm a "Meta-variables" section. Static: docs/manual/metavars.md has a meta-variables page ("Meta-variables expose row-level dimensions...") but its title/section name is not "Query your logs" (not checked further)

### Write, account or human

- [ ] **weftd-02** (doc line 12) weftd tails log files
  - On a Linux host with weftd installed: `echo hi >> /var/log/myapp/api.log`, then `weft sh -c "tail -n 3 /log/<ns>/<env>/api/main.log"`. Observed: dev stack ingests weftd containers (`weft source list` shows acme api/edge/worker TYPE weftd, STATUS configured; `tail /log/acme/prod/api/main.log` returns live lines)
- [ ] **weftd-06** (doc line 20) `sudo dpkg -i weftd_*.deb` installs on Debian/Ubuntu
  - Download the .deb from the Downloads page onto a Debian/Ubuntu host and run `sudo dpkg -i weftd_*.deb`; then `systemctl cat weftd`
- [ ] **weftd-07** (doc line 23) Config file is `/etc/weftd/config.yaml`
  - On installed host: `ls /etc/weftd/config.yaml`
- [ ] **weftd-08** (doc line 28) `endpoint` is optional, defaults to `ingest.w3ft.net:9020`
  - Omit `endpoint:` on a host; `ss -tnp \| grep 9020` (or `nc -vz ingest.w3ft.net 9020`) to confirm agent dials the default
- [ ] **weftd-15** (doc line 40-44) `dimensions:` column values sourced from files on disk, watched live via fsnotify; deploy touching file flows to next row without restart
  - On host: edit the identity.json file (e.g. change `.shard_id`), wait a few seconds with no restart, `weft sh -c "tail -n 3 <path> \| jq .node"` shows new value. Dev data: `jq -r .node` -> null (no dimensions.node configured there)
- [ ] **weftd-16** (doc line 45) `node: /etc/weft/identity.json:.shard_id` form works
  - On host: `echo '{"shard_id":"x"}' > /etc/weft/identity.json`, config `node: /etc/weft/identity.json:.shard_id`, restart, `tail <path> \| jq .node` expect "x"
- [ ] **weftd-17** (doc line 47-52) `sources:` entry fields: service, path (glob), schema, prefix_fields, prefix_separator, json_field
  - Configure a prefix_json source on a host, `weft sh -c "tail -n 3 <path> \| jq ."` and check ts/level/service/payload fields parsed
- [ ] **weftd-18** (doc line 51) `prefix_fields: [ts, level, service]`
  - as weftd-17
- [ ] **weftd-19** (doc line 54-57) `version_file` optional; weftd watches file and stamps latest contents as VERSION on every row from that source
  - On host: set `version_file`, change file, `tail <path> \| jq .version` then check the VERSION meta-var. Dev data: `jq -r .version` -> null (no version_file set)
- [ ] **weftd-20** (doc line 61) `schema: json` valid for nginx access.log
  - Host with nginx JSON access log; `tail` the nginx service
- [ ] **weftd-21** (doc line 64) Config is single source of truth; package never generates or overwrites it
  - On host: upgrade .deb with a customized config.yaml, `diff` before/after; expect identical
- [ ] **weftd-22** (doc line 65) `/etc/weftd/config.yaml.example` exists
  - On host after install: `ls /etc/weftd/config.yaml.example`
- [ ] **weftd-23** (doc line 66-68) YAML anchors and merge keys (`&name`, `<<: *name`) supported
  - On host: config using `&a` / `<<: *a`, `sudo systemctl restart weftd`, confirm `systemctl status weftd` active and rows flow
- [ ] **weftd-24** (doc line 67-68) weftd ignores `x-anchors:` holder block
  - On host: add `x-anchors:` top-level block, restart weftd, confirm no config error in `journalctl -u weftd`
- [ ] **weftd-26** (doc line 73) `service:` is optional
  - On host: omit `service:`, see weftd-28 for resulting name
- [ ] **weftd-27** (doc line 74) systemd source (`unit:`) default service = unit name without `.service`
  - On host: source `unit: foo.service` without `service:`, `weft sh -c "ls /log/<ns>/<env>"` shows `foo/`
- [ ] **weftd-28** (doc line 75-76) File source default service = filename without extension (`/var/log/myapp/api.log` -> `api`)
  - On host: source `path: /var/log/myapp/api.log` without `service:`, `ls /log/<ns>/<env>` shows `api/`
- [ ] **weftd-29** (doc line 76-77) Set `service:` explicitly to share one service across several files
  - On host: two path sources with same explicit `service:`; both flow to one service dir
- [ ] **weftd-30** (doc line 78-79) Top-level `services:` is separate and only controls host-metrics fan-out
  - On host: `services: [a,b]` vs source services; confirm `ls /log/<ns>/<env>/a` has metrics.log only
- [ ] **weftd-31** (doc line 83-84) `token:` in file OR `WEFTD_TOKEN` env var (e.g. `/etc/default/weftd`)
  - On host: remove `token:` from yaml, put `WEFTD_TOKEN=wft_...` in /etc/default/weftd, restart, confirm ingest authenticates
- [ ] **weftd-32** (doc line 85) Environment wins over file value
  - On host: set valid token in env and a bogus one in yaml (and vice versa), restart; the one that authenticates is the winner. (Only test with revocable audit tokens)
- [ ] **weftd-33** (doc line 87-88) Token is the only setting with an environment source
  - Needs host/source review; no black-box check
- [ ] **weftd-36** (doc line 97) `dimensions:` entry forms `<column>: <path>` or `<column>: <path>:.<json.path>`
  - On host: set both forms of `dimensions:` entries, `tail <path> \| jq .<column>`
- [ ] **weftd-37** (doc line 98-99) Value stamped on every row shipped from this host
  - As weftd-36
- [ ] **weftd-38** (doc line 99-101) Operator config wins over auto-derived (`dimensions.region` overrides metadata probe)
  - On host with cloud metadata: set `dimensions.region: /etc/region` and compare `jq .region` to instance region
- [ ] **weftd-39** (doc line 101-102) Allowed columns: deployment, version, region, cluster, node
  - On host: set `dimensions.foo: /tmp/x`, restart, expect config rejection in `journalctl -u weftd`
- [ ] **weftd-40** (doc line 102-105) Other columns not overridable (token-bound env, schema fields, k8s path metadata)
  - On host: set `dimensions.env: /tmp/x`, expect rejection
- [ ] **weftd-41** (doc line 107-110) JSON-path subset: `.foo`, `.foo.bar`, `.items[0].name` supported; richer unsupported
  - On host: file containing nested/array json; try `.items[0].name` and a richer expr like `.a \| length`; compare
- [ ] **weftd-42** (doc line 114) `json` schema = pure JSON lines, no prefix
  - Host with json schema source; `tail <path> \| jq .`
- [ ] **weftd-43** (doc line 115) `prefix_json` = fixed-width prefix fields + JSON payload
  - Host with prefix_json source
- [ ] **weftd-44** (doc line 116) `raw` = unstructured text, no parsing
  - Host with raw schema source
- [ ] **weftd-45** (doc line 120) `sudo systemctl enable --now weftd` starts agent
  - On host: `sudo systemctl enable --now weftd; systemctl is-active weftd`
- [ ] **weftd-46** (doc line 123-124) `sudo systemctl status weftd`; `journalctl -u weftd -f` show status/logs; unit name `weftd`
  - On host: `sudo systemctl status weftd`; `journalctl -u weftd -f`
- [ ] **weftd-47** (doc line 129-131) weftd remembers delivered position per tailed file across restart
  - On host: write line A, `sudo systemctl stop weftd`, write line B, start; `tail` both A and B present once
- [ ] **weftd-49** (doc line 134-135) Delivery at-least-once; after unclean stop may see few seconds of dupes, never a gap
  - On host: write steadily, `sudo kill -9 $(pidof weftd)`, restart; check no gap and a few seconds of dupes in `tail`
- [ ] **weftd-50** (doc line 137) State kept in `/var/lib/weftd/checkpoints/`
  - On host: `ls /var/lib/weftd/checkpoints/`
- [ ] **weftd-51** (doc line 137-138) `.deb` unit creates it via `StateDirectory=weftd`
  - On host: `systemctl cat weftd \| grep StateDirectory`
- [ ] **weftd-52** (doc line 138) All checkpoint settings optional
  - On host: remove `checkpoint:` block, restart, check journalctl for no error
- [ ] **weftd-53** (doc line 141-142) `checkpoint.dir` default `/var/lib/weftd/checkpoints`, holds `state.json`
  - On host: `ls /var/lib/weftd/checkpoints/state.json`
- [ ] **weftd-54** (doc line 143) `enabled: true` default; false = tail from end on every start
  - On host: `enabled: false`, append while stopped, restart, confirm backlog skipped
- [ ] **weftd-55** (doc line 144) `flush_interval` default 5s (write positions to disk)
  - On host: `stat -c %y state.json` twice ~5s apart
- [ ] **weftd-56** (doc line 145) `commit_interval` default 15s (ack delivered lines)
  - On host: watch `journalctl -u weftd` for ack cadence (~15s)
- [ ] **weftd-57** (doc line 146) `max_resume_bytes` default 256MiB per file
  - On host: stop weftd, append >256MiB, start; check journal for replay cap
- [ ] **weftd-58** (doc line 151-153) First start with checkpointing (no state) -> end of file
  - On host: stop/restart weftd per the table row (first-start) and confirm via `tail` + `journalctl -u weftd \| grep "checkpoint stats"`
- [ ] **weftd-59** (doc line 154) File with no saved position (appeared while down) -> beginning
  - On host: stop/restart weftd per the table row (new-file) and confirm via `tail` + `journalctl -u weftd \| grep "checkpoint stats"`
- [ ] **weftd-60** (doc line 155) File matches saved position -> saved position
  - On host: stop/restart weftd per the table row (matching) and confirm via `tail` + `journalctl -u weftd \| grep "checkpoint stats"`
- [ ] **weftd-61** (doc line 156) Truncated file -> beginning
  - On host: stop/restart weftd per the table row (truncated) and confirm via `tail` + `journalctl -u weftd \| grep "checkpoint stats"`
- [ ] **weftd-62** (doc line 157) Rotated-while-down file -> beginning; old tail not recovered
  - On host: stop/restart weftd per the table row (rotated) and confirm via `tail` + `journalctl -u weftd \| grep "checkpoint stats"`
- [ ] **weftd-63** (doc line 158) >max_resume_bytes behind -> newest max_resume_bytes from line boundary
  - On host: stop/restart weftd per the table row (over-cap) and confirm via `tail` + `journalctl -u weftd \| grep "checkpoint stats"`
- [ ] **weftd-65** (doc line 162) Files created while running read from first line
  - On host: with weftd running create a new file matching path, confirm first line appears in `tail`
- [ ] **weftd-66** (doc line 162-163) Every skip/replay is logged, summarized once a minute as `checkpoint stats` line per file
  - On host: `journalctl -u weftd \| grep "checkpoint stats"` after a restart
- [ ] **weftd-67** (doc line 167-169) First restart onto a checkpointing version still loses in-flight lines; guarantee from second restart
  - On host: upgrade from a pre-checkpoint weftd; write during first restart, expect loss; second restart none
- [ ] **weftd-68** (doc line 170-172) Kubernetes: mount `/var/lib/weftd` from hostPath; emptyDir wiped on reschedule
  - On Kubernetes: compare hostPath vs emptyDir across a pod reschedule
- [ ] **weftd-69** (doc line 173-174) Two weftd on one host can't share state dir; second logs "locked" and runs without checkpointing
  - On host: start two weftd with same state dir; second logs "locked"
- [ ] **weftd-70** (doc line 175-176) Damaged `state.json` moved to `state.json.corrupt`; weftd continues as fresh; never refuses to start
  - On host: `echo garbage > /var/lib/weftd/checkpoints/state.json`, restart; `ls` shows state.json.corrupt, service active
- [ ] **weftd-72** (doc line 180-182) Opt in via `metrics:` block attached to any source
  - Config inspection needs host: `grep -A8 metrics: /etc/weftd/config.yaml`; data-side effect verified by weftd-71
- [ ] **weftd-73** (doc line 182-183) Block independent of what source tails
  - Host: attach `metrics:` to a source tailing a different path than any metric service; metrics still flow
- [ ] **weftd-75** (doc line 195) `metrics.enabled: true`
  - Host: set `metrics.enabled: true`; metrics.log appears (observed on dev stack, which has metrics rows)
- [ ] **weftd-89** (doc line 223-225) cpu and network need two samples; first tick after startup silent for those two
  - On host: `sudo systemctl restart weftd`, tail metrics.log: first tick has memory/disk/load but no cpu/network rows
- [ ] **weftd-95** (doc line 245) On other kernels the block is ignored
  - Run weftd with a metrics block on a macOS host; confirm no metrics.log rows
- [ ] **weftd-96** (doc line 249-250) weftd exposes a local records socket for apps
  - On host: set `socket:`; `ls -l /var/run/weftd.sock`
- [ ] **weftd-97** (doc line 251-253) Binds to a Unix socket by default; no token/network/dependency needed
  - On host: `echo "x:hi" \| nc -U /var/run/weftd.sock` as unprivileged user without token
- [ ] **weftd-98** (doc line 253-254) Events, heartbeats, runtime stats all use this one channel
  - Host/SDK: send event, heartbeat, runtime stats over one socket; see rows
- [ ] **weftd-100** (doc line 259-261) `socket:` line in config enables socket; frames format `<app>:<line>\n`
  - On host: set `socket:`, `echo "billing:hello" \| nc -U /var/run/weftd.sock`, `tail` the billing service path
- [ ] **weftd-101** (doc line 261-263) Each line forwarded to same ingest path as file tailers, stamped with `<app>` prefix
  - As weftd-100; confirm same ingest path
- [ ] **weftd-102** (doc line 266) `socket: /var/run/weftd.sock`
  - On host: config line `socket: /var/run/weftd.sock`, restart, `ls` socket
- [ ] **weftd-103** (doc line 271) `echo "billing:user signup id=42" \| nc -U /var/run/weftd.sock` ships a line
  - On host: run the echo\|nc line, then `weft sh -c "ls /log/<ns>/<env>/billing"`. Note: path segments under /log are 4-seg; the doc elsewhere says `/log/<ns>/billing/...`
- [ ] **weftd-104** (doc line 274-278) Go snippet: `weft.New()`, `c.Close()`, `c.Send("billing", map[string]any{...})` valid API
  - `go doc github.com/w3ft-net/weft-go` (needs go module fetch of third-party repo; not run)
- [ ] **weftd-105** (doc line 281) SDK default socket `/var/run/weftd.sock`
  - `go doc github.com/w3ft-net/weft-go` or read repo source for default socket
- [ ] **weftd-106** (doc line 282) Override with `WEFT_SOCKET` or `weft.WithSocket(...)`
  - Repo README / `go doc ... WithSocket`
- [ ] **weftd-107** (doc line 286-287) Go SDK `Heartbeat` loop emits to app `<service>/heartbeats` every interval over records socket
  - Run a Go service with Heartbeat on a weftd host; `tail <path>/heartbeats.log`. Observed on dev stack: no heartbeats.log anywhere (`ls /log/acme/prod/api` -> main.log, metrics.log)
- [ ] **weftd-110** (doc line 292) Heartbeat record shape `{"type":"heartbeat","service":"my-service","ts":"..."}`
  - On host, send a heartbeat then `weft sh -c "tail -n 1 <heartbeats path>"` and compare shape
- [ ] **weftd-111** (doc line 297) `go c.Heartbeat(ctx, "my-service", weft.DefaultHeartbeatInterval)` valid
  - `go doc github.com/w3ft-net/weft-go Client.Heartbeat`
- [ ] **weftd-112** (doc line 300-301) Without SDK, `echo 'my-service/heartbeats:{...}' \| nc -U /var/run/weftd.sock` writes same record
  - On host: run the echo\|nc line; `tail` heartbeats.log shows the record
- [ ] **weftd-113** (doc line 304) Service names restricted to `[a-zA-Z0-9._-]`, 1-128 chars
  - On host: `echo "bad name!/x:y" \| nc -U ...` and a 129-char name; confirm rejection in journalctl
- [ ] **weftd-114** (doc line 306-308) Liveness judged by recency; stopped service is absent
  - Needs a heartbeating service: create cron (human step: `vi /cron.d/billing-beat`), stop beats, confirm `weft::is_absent billing-beat 90s` flips. Observed baseline: `weft::is_absent <existing cron> 90s` rc=1, `weft::is_absent nosuchcron 90s; echo rc=$?` rc=1 (no output either way)
- [ ] **weftd-119** (doc line 322-325) weft-go provides events, heartbeats, and runtime-stats collector emitting Go heap, GC pause, goroutine, FD-count
  - Read repo README / `go doc github.com/w3ft-net/weft-go`
- [ ] **weftd-120** (doc line 327-334) `go c.RuntimeStats(ctx, "my-service", weft.DefaultRuntimeStatsInterval)` valid
  - `go doc github.com/w3ft-net/weft-go Client.RuntimeStats`
- [ ] **weftd-122** (doc line 336-337) SDK prefers Unix socket, falls back to HTTP endpoint when `WEFT_ENDPOINT` + `WEFT_TOKEN` set
  - Repo README; or run SDK on host without socket and with WEFT_ENDPOINT/WEFT_TOKEN
- [ ] **weftd-123** (doc line 337-338) Repo README has full env-var table and option list
  - Open https://github.com/w3ft-net/weft-go README and look for env-var table (repo returns 200 per weftd-99; content not fetched)

