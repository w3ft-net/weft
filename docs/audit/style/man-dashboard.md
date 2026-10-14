# Style review: docs/shell/man-dashboard.md
Verified: ✅ 3 · ❌ 0 · ⚠️ 8 · 🔒 22 · 🖥 22 · ∅ 1   (recounted from ledger rows)
Audience: shell only (terminal full-screen view)   Words: ~360

## Pre-filled checklist
- [x] 1. Opens with what the reader accomplishes, not background
- [?] 2. Task-oriented sentence-case headings, no filler — all-caps man-page headings; several tiny key-list sections could merge.
- [ ] 3. Every code block is copy-pasteable and runs as written — l.36-58 reference crons that likely do not exist (13); l.92 `ops` does not exist (50).
- [ ] 4. Realistic names; no real customer/account data — `pending-jobs`, `top-errors`, `disk-pct`, `ops` are illustrative only.
- [x] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
- [?] 6. Sample output matches the verified output — no output shown; unverified (🖥).
- [x] 7. No internal vocabulary (warp, DuckDB, shard, ModeLess, repo paths)
- [ ] 8. Terminology — l.42, 45, 50, 56 use literal `watch:` key; l.77 calls it a cron without saying the key is literally `watch`, not `cron` (17, 36).
- [x] 9. No stale roadmap language or unshipped-feature promises; no Windows
- [?] 10. Voice — l.78-81 "kind-specific" wording implies rejection; ledger says wrong-kind keys are accepted silently (40-48).
- [x] 11. Cross-references name help/man topics (resolve statically; live check 🖥)
- [x] 12. Renders cleanly in both targets (static check found no stray {{ }} or &lt;)
- [?] 13. Length proportionate; no duplicate — page never mentions the web Dashboards page; terminal vs web terminology (team doc "Members tab").

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| 36/17 | 77 | `watch` — name of an existing cron (must exist when saved). | "`watch` — (the key is literally `watch`, not `cron`) name of an existing cron; must exist when the file is saved." |
| 13 | 36-58 | sample YAML, no caveat | add "Each `watch:` must name a cron you have already created (`ls /cron.d`)." |
| 43 | 80 | `threshold` — (counter, gauge, sparkline) `warn`, `crit`, and `max` cutoffs. | "`threshold` — (counter, gauge, sparkline) `warn` and `crit` cutoffs; gauge also takes `max`." |
| 50 | 92 | `dashboard ops` / `vi /etc/dashboards/ops.yaml` | confirm the example name is meant to be created first; add "create it first" (draft) |
| 55 | 31-32 | Create one with `vi /etc/dashboards/<name>.yaml` | add one line that writing /etc needs the `configmgr` group |
| 40-48 | 78-81 | `format`/`columns`/`max` "(X only)" | say wrong-kind keys are ignored, if confirmed (draft) |

## Needs your decision first (product vs doc)
- 36: the tile key is `watch`, user-facing name is cron. Rename or add alias `cron:` in product, or keep `watch` and document as literal.
- 40-48: wrong-kind keys (format on gauge, threshold on table) accepted silently per prior audit. Product should reject, or doc says "ignored".
- 55: `configmgr` required to write /etc/dashboards; doc silent. Document the permission or leave to the passwd page.

## Notes
- Only 3 rows verified; 22 🔒 and 22 🖥 need a human running `dashboard prod` in `weft sh`.
- EXAMPLES block uses aligned-column descriptions; other man pages use `# comment` style.
- Add a sentence linking to the web Dashboards page, if one exists, and name the difference.
- Merge TOP-LEVEL/SECTION/TILE/THRESHOLD KEYS into one reference table to shorten the page.

## Applied 2026-10-12
Changed: configmgr note; example crons must exist; watch is required and literally 'watch'; added title, tag/y keys; threshold max is gauge-only; columns required; examples 'create it first'. Left (product): wrong-kind keys silently accepted, watch->cron rename; literal watch: keys kept.
