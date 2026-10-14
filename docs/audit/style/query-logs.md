# Style review: docs/manual/query-logs.md
Verified: ✅ 42 · ❌ 6 · ⚠️ 26 · 🔒 9 · 🖥 1 · ∅ 11
Audience: customer-facing web+shell   Words: ~1190

## Pre-filled checklist
- [ ] 1. Opens with what the reader accomplishes, not background — L12-13 is OK but L32-34 closes on marketing ("makes them fast").
- [ ] 2. Task-oriented sentence-case headings, no filler — "Why two naming styles", "Meta-variables" are concept headings; acceptable but not task-oriented (L36, L79).
- [ ] 3. Every code block is copy-pasteable and runs as written — fails: query-logs-04/82 (bare `myapp.log`, L17-29, L162), -08/09 (`*.log` glob, L26), -91/92 (`range 6m..`, L177), -39 (L73).
- [ ] 4. Realistic names; no real customer/account data — `myapp`, `nginx`, `myco` mixed; `/log/weft/shard` (L73) leaks internal service names.
- [x] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
- [?] 6. Sample output matches the verified output — none shown; TS format differs awk vs jq (query-logs-24, L52); your call to document.
- [ ] 7. No internal vocabulary — "DuckDB shards", "Parquet", "row-group statistics", `json_extract_string`, "SQL WHERE" at L40-41, L103-105, L176-177.
- [x] 8. Terminology: cron (not watch), pseudofile, namespace, org, token
- [ ] 9. No stale roadmap language — "currently", "follow-up", "For now" at L123-125.
- [x] 10. Voice: 2nd person, present tense, active; no hedging on deterministic behavior
- [?] 11. Cross-references — "Saving results to /tmp section" (L166) has no link; `help builtins`/`man range` unchecked (🖥, query-logs-94/95).
- [ ] 12. Renders cleanly in both targets — table row L61 very long; L103-104 inline code wraps mid-string.
- [ ] 13. Length proportionate; no duplicate of another doc — "Per-tag values" (L109-127) duplicates Alerting strategies; "Saving a result set" duplicates tmp doc.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|----|------|----------|------------|
| query-logs-04/82 | 17, 20, 23, 29, 162 | `tail -f myapp.log`, `cat api.log` | `tail -f /log/acme/prod/api/main.log` (or `cd /log/<ns>/<env>/<svc>` first, then `main.log`) |
| query-logs-08/09 | 26 | `cat *.log \| grep "connection refused"` "across all sources" | `cat /log/acme/prod/api/*.log \| grep ...` (one service), or state the glob limits |
| query-logs-91/92 | 177-179 | "A `range 6m..` query stitches hot and archived shards" | "A `range 180d..` query" (`6m` is six minutes; there is no month unit) |
| query-logs-06 | 19 | "Search for errors in the last hour" | "Search for errors in the default range (last day; narrow with `range 1h..`)" |
| query-logs-35 | 45-61 | 15-row table presented as the indexed set | add `_duration_ns`, `_span_id`, `_span_name`, `_trace_id` rows, or say "among others" (draft) |
| query-logs-39 | 73 | `tail /log/weft/shard/metrics.log` | `tail /log/acme/prod/api/metrics.log` |
| layout | 47, 48, 90 | `/log/<namespace>/<service>/<kind>.log` (3 segments) | `/log/<namespace>/<env>/<service>/<kind>` (4 segments, as in the examples at L67-76) |
| query-logs-79 | 153-154 | "chart kind" | name the flag `--type` (no `--kind`) (draft) |
| style | 122-125 | "currently summarize... a follow-up. For now..." | "Window aggregates (`weft::avg`, `weft::max`) summarize the whole run's stdout as one number. For a rolling average across buckets, point a second cron at it via `weft::latest`." (draft) |
| style | 40-41, 103-105, 176 | "SQL WHERE clauses with row-group statistics", "json_extract_string", "hot DuckDB shards... Parquet on S3" | "Filtering on these columns is fastest"; "older data is archived and queried transparently" (draft) |

## Needs your decision first (product vs doc)
- query-logs-08/09: relative `cat *.log` returns empty silently and `/log/<ns>/<env>/*/main.log` errors. Make globs work across services, or document final-segment-only globs.
- query-logs-91/92: no month unit in `range`. Add `mo` unit to the product, or change the doc to `180d`.
- query-logs-65: `chart | cat` swallows output silently. Reject with an error, or leave and keep "terminal stage" wording.
- query-logs-24: awk `TS` and jq `._ts` print different formats. Unify in product, or document both.

## Notes
- Retention claims (Trial one day, Bolt one year, L171) are unverified (query-logs-85..89); plan names in the ledger disagree (`yard` vs `trial`).
- Move "Per-tag values over time" fully into Alerting strategies; leave a one-line pointer.
- Cron-dependent claims (L117-127) were untestable (cronmgr denied); recheck with a cron-capable user.
- Mixed 3-segment and 4-segment example paths (L73, L137, L67) confuse; pick the 4-segment layout throughout.

## Applied 2026-10-12 (docs/manual/query-logs.md, uncommitted)
- L19 comment now says default range (last day; `range 1h..`); default verified with `weft sh -c range` -> `[1d]`.
- L154 "chart kind" -> `--type` (man-chart documents `-t, --type`).
- L122-127: removed "currently / follow-up / For now" roadmap wording.
- L176-177: `range 6m..` -> `range 180d..` (verified `--range 180d..` accepted; `6mo` rejected; `6m` is six minutes); dropped DuckDB/Parquet/S3 vocabulary.
- L40-41, L103-105: dropped "row-group statistics", `json_extract_string` and SQL vocabulary.
- Not changed: bare `myapp.log` examples (illustrative; `<name>.log` is the namespace wildcard), `cat *.log` (product decision, glob fix not deployed), 3- vs 4-segment layout (both valid), indexed-columns table (query-logs-35, needs the span columns confirmed), retention claims (unverifiable).
- Generated files not regenerated; run `go generate` and build the frontend before committing.

## Applied 2026-10-12 (second pass)
- Replaced "planner pushes into SQL" with plain wording; added hard-line-count rule and "no -n in crons" (decision 1); 3-segment vs 4-segment sentence (decision 3).
- `/log/weft/shard/...` example -> `/log/myco/prod/api/metrics.log`; NODE example "shard ID" -> "workload ID".
- Plan retention numbers removed; points to Account page (decision 13).
- Left: bare `myapp.log` namespace-wildcard examples (valid), indexed-column table completeness, `help builtins` xref.
