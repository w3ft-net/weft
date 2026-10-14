# Style review: docs/manual/render.md
Verified: ✅ 2 · ❌ 1 · ⚠️ 11 · 🔒 22 · 🖥 2 · ∅ 10   (48 claims)
Audience: customer-facing web+shell   Words: ~690

## Pre-filled checklist
- [x] 1. Opens with what the reader accomplishes
- [x] 2. Task-oriented sentence-case headings, no filler
- [ ] 3. Every code block is copy-pasteable and runs as written
  - L63-67 paths omit env segment (render-24/25); L38 sample omits `Label:` line (render-12).
- [x] 4. Realistic names; no real customer/account data
- [ ] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
  - L99 **Tokens** has no reissue action (render-42); L50 `<producer>` placeholder undefined (render-17).
- [ ] 6. Sample output matches the verified output
  - L37-40 matches CLI strings but omits `Label: render` (render-12).
- [x] 7. No internal vocabulary (warp, DuckDB, shard, ModeLess, repo paths)
- [x] 8. Terminology: cron (kind `cron` is a Render process type)
- [ ] 9. No stale roadmap language; no Windows
  - L74 "an undocumented `bld`" is odd in a doc; L92-93 "Contact us" has no contact path.
- [ ] 10. Voice: 2nd person, present tense, active; no hedging on deterministic behavior
  - L29-31 contradicts L43 (token "stay visible" vs "only shown once").
- [?] 11. Cross-references name a help/man topic or title and resolve in both shell and web
  - **Namespaces** by title; L110 "weft's HTTP ingest endpoint" unnamed.
- [x] 12. Renders cleanly in both targets (L80 table cosmetic misalignment only)
- [?] 13. Length proportionate; no duplicate of another doc
  - Parallels heroku.md; Rate-limits section gives no number (heroku gives 1000/s).

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| render-09 | 29-31 | token and endpoint stay visible in token table | "The token is shown only once, so copy it now. The endpoint is the same for every Render token (`render.syslog.w3ft.net:6514`), so you can always look it up here." (confirm endpoint is constant) |
| render-12 | 37-38 | sample lacks `Label:` line | add `Label: render` after the Token line |
| render-24/25 | 63-67 | `ls /log/myco/myservice` | `ls /log/myco/prod/myservice` (if token has env) |
| render-32-34 | 80-82 | 3-segment table paths | add env segment or note env-less form |
| render-42 | 99 | "Reissue the token from **Tokens**" | "Revoke and recreate the token from **Tokens** (or `weft token revoke` / `weft token create`)." |
| render-17 | 50 | `<producer>.syslog.w3ft.net:6514` | `render.syslog.w3ft.net:6514` (draft) |
| render-46 | 107-111 | "weft's HTTP ingest endpoint" | name the endpoint (draft; none named in doc) |

## Needs your decision first (product vs doc)
- render-09: if the web token table does show the full token or endpoint, the product contradicts "shown once". Fix doc (CLI evidence) or fix product.
- render-24: confirm whether env-less Render tokens yield 3-segment paths; otherwise all 3-segment examples are stale.
- render-38/40/47: no default rate number, no contact path for higher rates or the Vector config. Supply, or cut the sentences.

## Notes
- render-04: only TCP connect to port 6514 was seen; TLS handshake unverified.
- L74 reword: "(`web`, `cron`, and `bld` for build-step output)".
- 10 claims are third-party (∅) and cannot be verified by weft; consider trimming Render-internals prose (L18-21, L53-58).

## Applied 2026-10-12
- Token-table claim replaced (token shown once; endpoint constant); `Label: render` line added; endpoint string is `render.syslog.w3ft.net:6514` (dev derives it from base URL); ls order alphabetical (cron.log, web.log); "undocumented" dropped; reissue -> revoke and recreate; env-less note added.
- Left: Vector config / higher-rate contact sentences, TLS/Render-side claims.
