# Style review: docs/manual/dashboards.md
Verified: ✅ 26 · ❌ 1 · ⚠️ 14 · 🔒 0 · 🖥 23 · ∅ 1
Audience: customer-facing web+shell   Words: ~960

## Pre-filled checklist
- [ ] 1. Opens with what the reader accomplishes, not background — L10-16 "What dashboards are" is definition-first; lead with creating one (L18-22).
- [ ] 2. Task-oriented sentence-case headings, no filler — "What dashboards are", "YAML grammar", "Deletion", "Validation" are noun headings; consider "Create a dashboard", "Delete a dashboard".
- [ ] 3. Every code block is copy-pasteable and runs as written — dash-07: example YAML is rejected until its four crons exist (L29-55); dash-05/53: `vi`/`rm` returned permission denied in the audit.
- [x] 4. Realistic names; no real customer/account data
- [x] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
- [?] 6. Sample output matches the verified output — no output samples; rendering claims are 🖥 (23 unverified).
- [x] 7. No internal vocabulary (warp, DuckDB, shard, ModeLess, repo paths)
- [ ] 8. Terminology: cron (not watch) — key `watch:` is shown 8 times (L35-141) without saying it names a cron; prose "the watch's last whitespace field" (L90), "/watch/<name>" (L161), "a watch's stdout" (L91).
- [x] 9. No stale roadmap language or unshipped-feature promises; no Windows
- [x] 10. Voice: 2nd person, present tense, active; no hedging on deterministic behavior
- [?] 11. Cross-references — no mention of Crons or `man dashboard` for the `watch:` key; "MCP server" section (L180) names no setup doc; your call.
- [ ] 12. Renders cleanly in both targets — `yes\*` with trailing footnote (L62-65) and long table cell (L188) may break in the terminal.
- [x] 13. Length proportionate; no duplicate of another doc

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|----|------|----------|------------|
| dash-08 | 35, 90-91, 161 | `watch:` and "the watch's last whitespace field" | "Each tile's `watch:` key names a cron (the key keeps its historical name)." |
| dash-07 | 29-55 | example presented as ready to save | add: "Create these crons first (the dashboard is rejected until they exist)" |
| dash-05/53 | 18-22, 167-171 | `vi`/`rm` with no group requirement | add: "Writing under `/etc` requires the `configmgr` group." |
| dash-25 | 83 | `format` "Applies to: counter" | "`format` is only used by counters and ignored elsewhere" |
| dash-26/37 | 84 | `threshold` "Applies to: counter, gauge, sparkline" | "`threshold` is ignored on table tiles" |
| dash-02/14 | 78-86 | tile fields table | add a `title` row (accepted key, used by the shipped prod.yaml) |
| dash-64 | 186-190 | `weft.dashboard_propose` ... | keep dotted names; optionally note that hosts such as Claude Code show them with underscores |
| dash-63 | 190 | "after human review" | "after human review in your MCP host" (draft; review is host-enforced) |
| style | 161 | "`/watch/<name>`" | name the cron detail page without the route, or show the literal and say it is the cron's page (draft) |
| style | 90-91 | "A sparkline normally plots one scalar per cron run (the watch's last...)" | "(the cron's last...)" (draft) |

## Needs your decision first (product vs doc)
- dash-05/53/54/59: shell `vi`/`rm` under `/etc` return permission denied for an admin listed in configmgr. Likely a minted-token artifact; recheck with a real login before touching either.
- dash-25/26: `format` on a gauge and `threshold` on a table are silently accepted. Reject at save, or keep and document as ignored.
- dash-08: key is `watch:`; `cron:` is rejected. Add a `cron:` alias in product, or keep `watch:` and document it.

## Notes
- 23 claims are 🖥 (layout, colors, legend, detail page); no column-by-column rendering was confirmed.
- L161 `/watch/<name>` is a literal route; keep, but name what it is.
- "Authoring via the MCP server" (L180-193) is for tool users, not dashboard readers; consider a short pointer instead.
- The proposed `title:` tile key should also appear in the YAML example.
- Keep `size` hint wording (L110-113); it matches the verified behavior.

## Applied 2026-10-12
Changed: configmgr note; watch key names a cron + create crons first; title row; prose 'watch' -> cron. Left: /watch/<name> literal route and watch: keys (rename), format/threshold wrong-kind ignored (product), MCP section, headings.
