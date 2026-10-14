# Ledger: dashboards.md

| id | line | claim | type | black-box test | verdict | evidence |
|---|---|---|---|---|---|---|
| dash-01 | 12-13 | A dashboard is a named view arranging existing crons into tiles | UI | Web UI Dashboards page; open a dashboard | 🖥 | Web UI; dashboard files exist (`ls /etc/dashboards` -> prod.yaml) |
| dash-02 | 13-16 | Each tile reads from a cron; no pipelines, template variables, or time-range pickers | UI | inspect dashboard in web UI for absence of controls; put `cmd:` in tile and expect rejection | ⚠️ | MCP propose with `cmd:` -> `'cmd' is not a valid key (expected one of: kind, watch, title, size, format, threshold, columns, tag, y)`; no pipeline tiles confirmed. UI absence of pickers not checked (🖥). Undocumented `title:` tile key accepted |
| dash-03 | 15-16 | Cron schedule and history define what tile shows | output | compare tile with `weft::latest <cron>` | ⚠️ | `weft::latest audit-dash-scalar` -> 3 (cron output); tile rendering itself not checked (🖥) |
| dash-04 | 18 | Dashboards live as YAML files under `/etc/dashboards/` | path | `ls /etc/dashboards` | ✅ | `ls /etc/dashboards` -> prod.yaml |
| dash-05 | 22 | `vi /etc/dashboards/prod.yaml` creates a dashboard | command | run it, save, `ls /etc/dashboards` | ⚠️ | `vi` absent non-interactively (`which: no vi in PATH`); redirect write `echo > /etc/dashboards/x.yaml` -> permission denied (configmgr) despite alice listed in configmgr; MCP propose+apply created files at /etc/dashboards/audit-dash-*.yaml |
| dash-06 | 25 | Any number of dashboards per customer, distinguished by name | number | create several (e.g. 50) | ∅ | Created 5 dashboards only (not 50); no limit seen |
| dash-07 | 29-55 | Example YAML (title Production, 2 sections, tiles sparkline/table/counter/gauge) is valid | output | save with crons error-rate-hourly, top-errors-hourly, pending-jobs, cpu-usage existing | ✅ | Verbatim example rejected: `dashboard references unknown watches: error-rate-hourly, top-errors-hourly, pending-jobs, cpu-usage` (expected since crons absent). Same structure with existing crons staged+applied OK; `ls /etc/dashboards` showed it [re-triaged 2026-10-12: doc says crons must exist and the dashboard arranges 'existing crons'; made-up names rejected as documented. Prior run with real crons (error-rate, mem-pressure) staged+applied OK; `ls /cron.d` -> error-rate mem-pressure.] |
| dash-08 | 31,34,35 | Tile field named `watch:` references the cron (legacy name; user-facing surfaces say cron) | UI | check whether `cron:` accepted; check error messages wording | ✅ | `watch:` accepted; `cron:` rejected: `'cron' is not a valid key (expected one of: kind, watch, title, ...)`. Doc keeps `watch:` as key (consistent), but prose calls it cron without saying key is `watch` |
| dash-09 | 37 | sparkline tile `size: md` valid | output | in example | ✅ | `size: md` sparkline staged+applied |
| dash-10 | 40 | `columns: [host, count]` flow-list syntax valid | output | in example | ✅ | `columns: [host, count]` flow list staged+applied |
| dash-11 | 47 | counter `format: "%d jobs"` renders e.g. "42 jobs" | output | view tile | 🖥 | `format: "%d jobs"` accepted on counter (applied); rendering needs UI |
| dash-12 | 48 | `threshold: { warn: 100, crit: 500 }` flow-map valid | output | save | ✅ | `threshold: { warn: 100, crit: 500 }` staged+applied |
| dash-13 | 53 | gauge threshold with max: 100 | output | save | ✅ | `threshold: { warn: 70, crit: 90, max: 100 }` on gauge staged+applied |
| dash-14 | 59-61 | Top-level `title` optional; heading at top of dashboard | output | omit title, save; set title and view | ⚠️ | Applied files with and without `title:`; heading rendering needs UI. `title:` omitted accepted (audit-dash-nt) |
| dash-15 | 62 | `sections` required (unless tiles) list of grouped tile sections | output | save file with neither: expect rejection | ✅ | Body `title: x` only -> `dashboard has no tiles`; sections with `tiles: []` -> `section 0 ("A") has no tiles` |
| dash-16 | 63 | Top-level `tiles` shorthand: flat list = one untitled section | output | save example at l.135-142 | ✅ | Flat `tiles:` dashboards staged+applied (audit-dash-nt, quick) |
| dash-17 | 65 | Use either `sections:` or `tiles:` not both | output | save with both: expect rejection | ✅ | sections+tiles -> `dashboard cannot mix top-level `tiles:` with `sections:` — pick one` |
| dash-18 | 71 | sparkline: time-series glyph from cron's recent results; requires `watch` | output | view tile; omit watch expect rejection | ⚠️ | sparkline w/o watch -> `section 0 tile 0: missing `watch:``; glyph rendering 🖥 |
| dash-19 | 72 | counter: latest numeric value; requires `watch` | output | view | 🖥 | counter accepted; rendering needs UI. `weft::latest` returns 3 |
| dash-20 | 73 | gauge: bar 0..threshold.max (default 100); requires `watch` | output | view with/without max | 🖥 | gauge accepted with/without max (q2 w/o threshold staged); rendering needs UI |
| dash-21 | 74 | table: multi-row stdout parsed as whitespace-split columns; requires `watch` and `columns` | output | omit columns expect rejection | ⚠️ | table w/o columns -> `table tile requires `columns:``; whitespace-split rendering 🖥 |
| dash-22 | 80 | `kind` one of sparkline, counter, gauge, table | output | `kind: bar` expect rejection | ✅ | `kind: bar` -> `unknown kind "bar" (valid: sparkline, counter, gauge, table)` |
| dash-23 | 81 | `watch` names an existing cron, must exist when the file is saved | output | reference nonexistent cron: expect rejection | ✅ | `watch: nonexistent-cron` -> `dashboard references unknown watches: nonexistent-cron` |
| dash-24 | 82 | `size`: sm, md, lg; default md; hint not pixel size | default | omit size; compare with explicit md; `size: xl` expect rejection | ✅ | `size: xl` -> `unknown size "xl" (valid: sm, md, lg)`; `LG` also rejected (case-sensitive); omitted size accepted. Default md/rendering 🖥 |
| dash-25 | 83 | `format` applies to counter only; `%d` / `%.2f` | output | counter format `%.2f`; format on gauge expect rejection/ignored | ⚠️ | `format: "%.2f"` on a gauge was accepted and staged (no rejection, `format: 5` non-string on counter also accepted). Doc says applies to counter only; silently ignored/accepted undocumented |
| dash-26 | 84 | `threshold` applies to counter, gauge, sparkline; keys warn, crit, max | output | threshold on table expect rejection? | ⚠️ | `threshold:` on a table tile accepted+staged (no rejection); doc table says counter/gauge/sparkline only. Threshold on sparkline accepted (applied) |
| dash-27 | 85 | `columns` for table: header labels; whitespace-split fields | output | table with cron emitting "h1 3\nh2 4" | 🖥 | `columns: [a, b]` accepted; header rendering needs UI. Cron `audit-dash-table` emitted `40 7e4ae977dce2 3` |
| dash-28 | 86 | `tag` and `y` apply to sparkline, must be set together | output | set only `tag:` expect rejection | ✅ | only `tag:` -> ``tag:` and `y:` must be set together (tag identifies which line a row belongs to, y is the field to plot)` |
| dash-29 | 90-92 | Plain sparkline plots one scalar per run: the watch's last whitespace field | output | cron emits "a b 7" -> plotted 7 | 🖥 | Cron emitted scalar; plot rule needs UI |
| dash-30 | 92-95 | Multi-row JSON stdout per run (one per tag value); `tag:` identifies row, `y:` field to plot; one line per distinct tag | output | cron emitting JSONL per instance; view | 🖥 | JSONL cron `audit-dash-jsonl` emits one JSON row per memory event; `tag: .host`/`y: .pct` sparkline accepted; chart needs UI |
| dash-31 | 97-103 | YAML with `tag: .coordinator_id`, `y: .mem_pct` (jq-style leading-dot paths) valid | output | save | ✅ | `tag: .host`, `y: .pct` (leading-dot) sparkline staged+applied |
| dash-32 | 105-107 | Row's x value is its run's timestamp (not per-row timestamp) | output | inspect chart x axis | 🖥 | x-axis behavior needs UI |
| dash-33 | 107-108 | Lines get a legend and a distinct color per tag | UI | view in web UI | 🖥 | legend/colors need UI |
| dash-34 | 108 | `tag:`/`y:` rejected on any kind other than sparkline | output | add to counter; expect rejection | ✅ | `tag:`/`y:` on counter -> `` `tag:`/`y:` are only supported on sparkline tiles`` |
| dash-35 | 110-113 | Renderers (browser/terminal) map size hint to viewport | UI | view in web and terminal | 🖥 | needs web + terminal render |
| dash-36 | 112-113 | Wide browser: `lg` tile spans full row; narrow terminal: single column | UI | resize web window / terminal | 🖥 | needs resize in browser/terminal |
| dash-37 | 117 | counter, gauge, sparkline accept `threshold:` map | output | as dash-26 | ⚠️ | threshold accepted on counter, gauge(example) and sparkline; also on table (see dash-26) |
| dash-38 | 123-124 | Values >= crit render red; >= warn and < crit yellow | UI | counter with threshold; values at boundaries; screenshot | 🖥 | color rendering needs UI |
| dash-39 | 124-125 | gauge uses threshold.max as full scale, default 100 | output | gauge w/ max 50 | 🖥 | gauge scale needs UI |
| dash-40 | 120 | `threshold: { warn: 70, crit: 90 }` without max valid | output | save | ✅ | `threshold: { warn: 70, crit: 90 }` w/o max staged+applied (audit-dash-tag variant sparkline) |
| dash-41 | 129-131 | Each section gets its own heading and grid | UI | view multi-section | 🖥 | multi-section applied; headings need UI |
| dash-42 | 133 | Flat top-level `tiles:` if no sections | output | save | ✅ | Flat `tiles:` staged+applied |
| dash-43 | 135-142 | Quick view example with counter pending-jobs and sparkline error-rate valid | output | save | ✅ | Quick-view example (substituting existing cron for pending-jobs; `error-rate` exists) staged+applied. Verbatim `pending-jobs` cron absent in this org |
| dash-44 | 146-148 | Renderer flows tiles row-major, per viewport and size hint | UI | view | 🖥 | UI |
| dash-45 | 150-152 | Browser CSS grid: 6 tracks wide, 2 on tablet, 1 on phone | number | web UI devtools/responsive resize; inspect grid-template-columns | 🖥 | needs browser devtools |
| dash-46 | 151-152 | sm spans 2 tracks, md 3, lg 6 | number | inspect grid-column span | 🖥 | needs browser devtools |
| dash-47 | 153-154 | Terminal: greedy row packing, kind-specific min widths | UI | view in terminal | 🖥 | needs terminal render |
| dash-48 | 154 | Below ~60 columns terminal collapses to one tile per row | number | resize terminal below 60 cols | 🖥 | needs terminal resize |
| dash-49 | 156-157 | No drag-and-drop; no per-tile pixel positioning | UI | try dragging in web UI | 🖥 | UI |
| dash-50 | 157 | YAML order is screen order | UI | reorder tiles | 🖥 | UI |
| dash-51 | 161 | Tile headers link to cron's detail page `/watch/<name>` (legacy route name) | path | click tile header; check URL | 🖥 | click needed |
| dash-52 | 161-163 | Detail page shows full history with zoom, range selection, per-tag legend | UI | open the page | 🖥 | UI |
| dash-53 | 167-170 | `rm /etc/dashboards/old.yaml` deletes dashboard | command | create then rm; verify gone from UI | ❌ | `rm /etc/dashboards/audit-dash-nt.yaml` -> `rm: ...: /etc: permission denied (need the configmgr group — ask an owner for `adduser <you> configmgr`)` for alice (admin, listed in configmgr in /etc/passwd). Deletion worked only via MCP `weft.proposal_delete`+apply. Possibly mint.py token lacks group claims (env) |
| dash-54 | 175-176 | Saving checks YAML parses | output | save garbage YAML expect error | ⚠️ | Garbage YAML via propose -> `yaml: yaml: did not find expected key`; shell write path untestable (permission denied) |
| dash-55 | 175-176 | Saving checks every `kind` recognized | output | as dash-22 | ✅ | see dash-22 |
| dash-56 | 176 | Checks every `size` is sm/md/lg | output | as dash-24 | ✅ | see dash-24 |
| dash-57 | 176-177 | Checks every table tile has `columns` | output | as dash-21 | ✅ | see dash-21 |
| dash-58 | 177 | Checks every referenced cron exists | output | as dash-23 | ✅ | see dash-23 |
| dash-59 | 177-178 | Bad file rejected at write time; previous version stays intact | output | save valid, then invalid; `cat` shows valid | ⚠️ | Rejection works on propose path (nothing written); shell write-time rejection untestable (permission denied) |
| dash-60 | 182-183 | Weft MCP server allows Claude Code/any MCP host to stage dashboards via review-and-apply pipeline same as scripts and crons | command | MCP: `weft_dashboard_propose`, `weft_proposals_list`, `weft_proposals_apply` | ✅ | `weft.dashboard_propose`, `weft.proposals_list`, `weft.proposals_apply` staged+applied dashboards (ids 0017-0020 applied) |
| dash-61 | 188 | `weft.dashboard_propose` stages new/updated dashboard, validates YAML + cron refs, returns unified diff against existing version | command | MCP `weft_dashboard_propose` with bad cron ref (expect error) and with edit of existing (expect diff) | ✅ | Bad cron ref -> `dashboard references unknown watches: nonexistent-cron`; edit of existing audit-dash-nt returned diff `@@ -1,3 +1,4 @@ +title: T` |
| dash-62 | 189 | `weft.proposal_delete` stages delete with `kind: dashboard, name: <n>` | command | MCP `weft_proposal_delete {kind: dashboard, name: x}` | ✅ | `weft.proposal_delete {kind: dashboard, name: audit-dash-ex}` -> `proposal 0012 staged (delete/audit-dash-ex)`; applied, file gone; nonexistent name -> `dashboard "audit-dash-nonexist" does not exist` |
| dash-63 | 190 | `weft.proposals_apply` commits staged proposals after human review | command | MCP `weft_proposals_apply` | ⚠️ | `weft.proposals_apply {ids:[...]}` applied; but the staging list is shared across clients (other agents' proposals 0001-0011 visible) and `ids` is required; "after human review" is host-enforced, not tool-enforced |
| dash-64 | 186-190 | Tool names written with dots (`weft.dashboard_propose`); actual MCP names use underscores `weft_dashboard_propose` | xref | `list` MCP tools; compare names | ⚠️ | `./weft mcp` tools/list names are DOTTED (`weft.dashboard_propose`, ...); `weft_dashboard_propose` is only the host-side sanitized name (calling underscore name directly -> `unknown tool "weft_crons_list"`). Doc is correct; ledger suspect was wrong |
| dash-65 | 192-193 | Cron refs checked against live cron list; can't stage dashboard pointing at nonexistent cron | command | MCP propose with missing cron expect rejection | ✅ | Same as dash-23 via propose: `dashboard references unknown watches: nonexistent-cron`; verified against `weft.crons_list` (error-rate, mem-pressure) |

### Style observations
- Pervasive use of `watch:` as the YAML key and `/watch/<name>` route (l.161) while prose says "cron": obsolete-name leak (l.15 even: "the watch's last whitespace field").
- l.90-91 "the watch's last whitespace field" / "a watch's stdout": uses "watch" as a noun.
- MCP tool names shown with dots vs underscore actual names (l.188-190).
- l.188 table cell very long; breaks table readability.
- Table col at l.63 uses `\*` footnote; fine.
- l.12 "tiles", l.129 "grid" vs l.153 terminal "packing"; fine.
- No warp, Windows, or "coming soon" mentions.

Summary: dashboards.md, 65 claims

## Findings

- **dash-53 / dash-05 / dash-54 / dash-59** (l.18-22,167-178): doc says `vi`/`rm` on /etc/dashboards works and validates at save. Observed: any shell write/rm as alice (admin) -> `/etc: permission denied (need the configmgr group...)` although /etc/passwd lists alice in configmgr. Likely env (minted JWT lacks group claims) or product bug; unverified. Also doc never mentions the configmgr requirement. Recommend: add a line "Writing under /etc requires the `configmgr` group."; recheck with a real login token.
- **dash-25 / dash-26 / dash-37** (l.83-84): `format` on a gauge and `threshold` on a table are accepted with no error or warning, and non-string `format: 5` is accepted. Doc's "applies to" column reads as a restriction. Doc/product ambiguous. Suggested text: "`format` is only used by counters and ignored elsewhere; `threshold` is ignored on table tiles."
- **dash-07** [re-triaged 2026-10-12: downgraded to ✅; doc already says crons must exist, names are illustrative] (l.29-55): example references crons (error-rate-hourly, top-errors-hourly, pending-jobs, cpu-usage) that do not exist, so verbatim save is rejected; reader may be confused. Suggested: add "Create these crons first (the dashboard is rejected until they exist)".
- **dash-08** (l.15,90): doc says "cron" everywhere but the key is `watch:` and `cron:` is rejected; the prose "the watch's last whitespace field" leaks the old name. Suggested: "Each tile's `watch:` key names a cron (the key keeps its historical name)."
- **dash-02/dash-14**: tile accepts an undocumented `title:` key (used by the shipped prod.yaml); table of tile fields omits it. Add a `title` row.
- **dash-63**: staging store is shared per user/account; `weft.proposals_apply` with a stale/other id would apply someone else's proposal. Doc fine; note that review is host-enforced.
- **dash-64**: doc's dotted names are correct for `weft mcp`; hosts (e.g. Claude Code) may expose them with underscores. Optionally note this in the doc.
