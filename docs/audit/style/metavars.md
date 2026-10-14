# Style review: docs/manual/metavars.md
Verified: ✅ 13 · ❌ 5 · ⚠️ 22 · 🔒 8 · 🖥 1 · ∅ 10
Audience: customer-facing web+shell   Words: ~960

## Pre-filled checklist
- [ ] 1. Opens with what the reader accomplishes, not background — L12-16 explains internals (json_extract, row-group stats) instead of what you can filter on.
- [ ] 2. Task-oriented sentence-case headings, no filler — "Reserved set", "Naming convention", "Pushdown" are concept/internal headings (L18, L42, L107).
- [ ] 3. Every code block is copy-pasteable and runs as written — fails: metavars-36/29 (`*/` dir globs, L98-99, L151), -44 (`POD ~ "..."`, L114), -55/57 (recipes on text `main.log`, L151-167).
- [ ] 4. Realistic names; no real customer/account data — `myco` is fine; mixed env-less `/log/myco/api/...` vs env paths confuse (L97, L140, L163).
- [x] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
- [ ] 6. Sample output matches the verified output — L90-95 mixes `weft>` prompt and `←` annotations into output block (metavars-33).
- [ ] 7. No internal vocabulary — "shard", "WHERE", "DuckDB", "json_extract", "row-group", `os.Hostname()`, `/var/log/pods` at L13-16, L25, L33, L109-128, L157.
- [x] 8. Terminology: cron (not watch), pseudofile, namespace, org, token
- [x] 9. No stale roadmap language or unshipped-feature promises; no Windows
- [?] 10. Voice: mostly descriptive third person; L50-54 awk digression is hedge-free but not task-voiced; your call.
- [?] 11. Cross-references — none to Alerting strategies or `man alert` from the recipes (L144-146); your call.
- [ ] 12. Renders cleanly in both targets — `→` arrow blocks (L111-124) and table cell with italic aliases (L25) may break; L10 `# Meta-variables` duplicates frontmatter title.
- [ ] 13. Length proportionate; no duplicate of another doc — the 15-variable table and naming rationale duplicate Query your logs (L36-92 there).

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|----|------|----------|------------|
| metavars-19 | 38-40 | "A row that wasn't tagged with a given dimension stamps `NULL` — queries on `ENV == "prod"` neither include nor exclude such rows" | "A row that wasn't tagged with a given dimension carries an empty string. `ENV == "prod"` does not match it, but `ENV != "prod"` does." |
| metavars-36 | 98-99 | `cat /log/myco/*/api/main.log` "matches across every env" | list the paths: `cat /log/myco/prod/api/main.log /log/myco/staging/api/main.log` |
| metavars-55 | 151 | `cat /log/myco/prod/*/main.log \| jq ...` | `cat /log/myco/prod/api/main.log /log/myco/prod/worker/main.log \| jq ...` (draft; one path per service) |
| metavars-44 | 114 | `awk 'POD ~ "api-7f9b"'` | `awk 'POD ~ /api-7f9b/'` |
| metavars-52 | 144 | "Each tick emits one row per host." | "Each tick emits one `<host> <pct>` row per memory sample; `-t 1` tags each row by its host." |
| metavars-54 | 145-146 | "page per host independently because `HOST` is a real column the alert grouping understands" | "page per host independently because `-t 1` tags each row by its first field (the host)." |
| metavars-51 | 139 | `# schedule: every 1m` | `# schedule: */1 * * * *` (existing crons use this form) |
| metavars-55/57 | 140-167 | recipes read plain-text `main.log` with `jq` | read `metrics.log`, or note "for JSONL streams" |
| metavars-33 | 90-95 | `weft> ls /log/myco` with `←` notes inside block | plain command, then output, with annotations moved to prose (draft) |
| layout | 73-74, 80-88 | `<kind>.log` and "Every segment is always present" | align with verified 4-segment `/log/<ns>/<env>/<svc>/<kind>`; mark env-less 3-segment as unverified (draft) |

## Needs your decision first (product vs doc)
- metavars-F1: `tail -n 100 main.log | awk 'LEVEL=="ERROR"'` filters before `tail`, so counts differ from grep. Fix the planner, or doc cannot promise pushdown semantics.
- metavars-36/29: directory-level globs (`*/`) fail with `no pseudofiles match`. Implement dir globs, or drop `*/` examples from the doc.
- metavars-44: awk `X ~ "string"` never matches; only `/regex/` works. Fix the interpreter, or document the regex-literal form.
- metavars-18: unlisted names like `FOO` evaluate to empty silently. Warn on unknown meta-variables, or accept as awk behavior.

## Notes
- Env-less 3-segment layout (L73, L83-88, L94) was untestable; consider cutting it if only 4-segment is supported.
- Move "Pushdown" (L107-128) to an internals page or drop; customers need the speed guidance only.
- heartbeats/runtime kinds (L24, L77-79) are absent from dev data; confirm they ship.
- `DuckDB` is named at L127; also "shard" at L15 and L157.
- Two docs define the same table; pick one home and cross-reference by title.

## Applied 2026-10-12
- Intro, "Pushdown" -> "Speed": dropped json_extract/row-group/shard/SQL/DuckDB vocabulary, os.Hostname and /var/log/pods; added hard-line-count rule (decision 1).
- NULL -> empty string (metavars-19). Path layout: 3-segment reads the service across envs, `ls` lists kinds, globs refused (decisions 2, 3).
- Recipes: cron recipe has no `-n`, says -n can truncate, `# schedule: */1 * * * *`, reads 4-segment metrics.log; per-host wording fixed (-t 1); env/region recipes rewritten with awk (jq on text rows errors), all re-run on real names.
- Left: reserved-table duplication with query-logs, H1 duplicate, heartbeats/runtime kinds, unknown-metavar warning.
