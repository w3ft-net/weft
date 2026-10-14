# Style review: docs/shell/man-chart.md
Verified: ✅ 26 · ❌ 0 · ⚠️ 12 · 🔒 0 · 🖥 13 · ∅ 2   (recounted from ledger rows)
Audience: shell only (terminal rendering; web shell follows)   Words: ~700

## Pre-filled checklist
- [x] 1. Opens with what the reader accomplishes, not background
- [?] 2. Task-oriented sentence-case headings, no filler — all-caps man-page headings; keep if that is the man convention.
- [ ] 3. Every code block is copy-pasteable and runs as written — l.98-111 (ledger 46-49): 3-segment paths, pretty-printed `jq` fails (F6, F7).
- [ ] 4. Realistic names; no real customer/account data — `/log/myco/api/main.log` and `/log/myco/coordinator/runtime_stats.log` do not match the real layout (l.98, 102, 106, 109).
- [x] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
- [?] 6. Sample output matches the verified output — no sample output at all; l.92-95 comments describe renders.
- [x] 7. No internal vocabulary (warp, DuckDB, shard, ModeLess, repo paths)
- [ ] 8. Terminology — doc says cron correctly, but product usage/error text says "watch" (F4); doc could not show the literal.
- [x] 9. No stale roadmap language or unshipped-feature promises; no Windows
- [ ] 10. Voice — l.44-45 "period." is colloquial; l.58-60 document `--type`/`--limit`/min interval that are not honored in cron form (F1, F2).
- [?] 11. Cross-references — l.121 `help cron` unverified (52); `alert -k` mentioned l.49 but `man alert` missing from SEE ALSO.
- [x] 12. Renders cleanly in both targets (static check found no stray {{ }} or &lt;)
- [ ] 13. Length proportionate — l.81-85: `--bucket`/`-t` defaults restated and slightly circular; cron default `line` vs pipe default `bar` is confusing.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| F5 | 97 | `# Pipe form: a quick distribution by host` (groups by `.path`) | "a quick distribution by path" (or use `.host`) |
| F6 | 98 | `cat /log/myco/api/main.log \| jq '{path, latency}' ...` | `/log/myco/prod/api/metrics.log` JSON example (4-segment) |
| F6 | 102-103 | `/log/myco/api/main.log ... -y .latency_ms` | 4-segment path to a JSON log with that field |
| F6/F7 | 106 | `cat /log/myco/api/main.log \| jq '.cpu_pct' \| chart` | JSON metrics path; `jq 'select(.cpu_pct) \| .cpu_pct'` (fails with no data when a row is null) |
| F6/F7 | 109-111 | `/log/myco/coordinator/runtime_stats.log \| jq 'select(...)'` | 4-segment path; `jq -c 'select(.metric=="mem_pct")'` |
| F4 | 47-49 | last whitespace field of cron stdout | add "The cron's stdout must end in a number on each line; crons that emit JSON rows (e.g. alert -k) cannot be charted." |
| F1 | 55, 58 | `-t`/`--type`, `-n`/`--limit` | "`-t <line\|sparkline\|bar>`, `-n <N>` (short forms only in cron form)" until product is fixed |
| F3 | 37-38 | terminal of the pipeline | add "Piping chart's output onward is an error." |
| 19 | 49 | matches `alert -k <N>` | questionable; mem-pressure cannot be charted. Reword or add `man alert` to SEE ALSO (draft) |

## Needs your decision first (product vs doc)
- F1: cron form silently ignores `--type`, `--limit`, `--limit=5`, `-n5`, `-t pie`. Fix flag parser, or document short forms only.
- F2: `-i abc` and `-i 50ms` accepted; 100ms minimum not enforced (needs TTY retest). Enforce in product or drop the claim.
- F7: multi-line pretty `jq` output breaks `chart` (`x: invalid json`). Make chart accept it, or require `jq -c` in docs.
- F4: usage and error text say "watch"; the product string should say cron.
- 11: `chart | cat` reports `chart: command not found`; improve the message, or leave it.

## Notes
- No example shows output; consider one rendered sample per kind.
- Placeholder style is mixed (`-t TYPE`, `<field>`, `<DURATION>`, `<N>`); pick one.
- 🖥 items (follow mode, keys, resize, `ls -l /cron.d/` sparkline) are unverified and need a human in `weft sh`.
- Cron-form default `line` vs pipe default `bar` deserves a small table.

## Applied 2026-10-12
Changed: examples now use /log/myco/prod/api/metrics.log with jq -c (F5/F6/F7; host grouping, p95, plain stream, --tag); added 'piping chart onward is an error' (F3); cron stdout must end in a number (F4). Left: F1 flag parsing, F2 interval minimum, 'period.' voice, placeholder style, no rendered samples.
