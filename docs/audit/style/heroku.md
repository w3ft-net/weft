# Style review: docs/manual/heroku.md
Verified: ✅ 3 · ❌ 2 · ⚠️ 13 · 🔒 29 · 🖥 4 · ∅ 5   (56 claims)
Audience: customer-facing web+shell   Words: ~620

## Pre-filled checklist
- [x] 1. Opens with what the reader accomplishes
- [x] 2. Task-oriented sentence-case headings, no filler
  - Heading "Metric lines auto-route to `/metrics`" (L79) is reference-ish and names a non-existent path (see 3).
- [ ] 3. Every code block is copy-pasteable and runs as written
  - L28 `issued:` (heroku-09); L50-57, L100 omit env segment (heroku-19/20/42).
- [x] 4. Realistic names; no real customer/account data
- [ ] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
  - L107 **Tokens** bolded as UI but no such nav item introduced (L23 says Namespaces).
- [ ] 6. Sample output matches the verified output
  - L28 prints `Token created:` plus Label/Env lines, not `issued:`.
- [x] 7. No internal vocabulary (warp, DuckDB, shard, ModeLess, repo paths)
- [ ] 8. Terminology: cron (not watch), pseudofile, namespace, org, token
  - L62 "sub-app" and L110 "app" vs "service" used elsewhere.
- [x] 9. No stale roadmap language; no Windows
- [x] 10. Voice: 2nd person, present tense, active (minor: L13-14 "within seconds" is unverified, 🔒)
- [?] 11. Cross-references name a help/man topic or title and resolve in both shell and web
  - **Namespaces** by title only; `man namespaces` exists (heroku-23, 🖥).
- [x] 12. Renders cleanly in both targets
- [?] 13. Length proportionate; no duplicate of another doc
  - Parallels render.md structure; mint-token step duplicated across Heroku/Render/CloudWatch.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| heroku-09 | 28 | `issued: wft_a1b2c3d4e5f6...` | `Token created: wft_a1b2c3d4e5f6...` / `Label: heroku` / `Env:   prod (stamped on every row from this token)` |
| heroku-19/20 | 50 | `ls /log/myco/myapp` | `ls /log/myco/prod/myapp` (token minted with `--env prod`) |
| heroku-21 | 57 | `tail /log/myco/myapp/web.log` | `tail /log/myco/prod/myapp/web.log` |
| heroku-31 | 70-74 | table paths `/log/<ns>/myapp/...` | add env segment, with note that env-less tokens give the 3-segment form |
| heroku-42 | 100 | `/log/myco/myapp/metrics.log` | `/log/myco/prod/myapp/metrics.log` |
| heroku-36 | 79, 88 | `/metrics`, `<appname>/metrics` | `<appname>/metrics.log` |
| heroku-46 | 107 | "reissue it from **Tokens**" | "revoke it and mint a new one (see **Tokens**)" |
| heroku-47 | 108-109 | "A `4xx parse error`" | "a `404`" (both trailing slash and missing prefix return 404) |

## Needs your decision first (product vs doc)
- heroku-05/06 (L22): "single namespace" token, but `token list` shows `acme,globex`. Reword doc, or confirm multi-namespace tokens are not meant for customers.
- heroku-19: example listing order and files are unverified; env-less paths work only if such tokens exist. Show env-bound examples, or explain both.
- heroku-55 (L118): "talk to us" has no contact path. Add a contact, or drop the sentence.

## Notes
- Example mixes `weft>` prompt (L50, 57, 100) with bare commands elsewhere; pick one.
- L116 "Heroku retries ... with backoff" is a third-party claim (heroku-53, ∅); consider softening or citing.
- Heroku data was never ingested in the audit; 29 claims await a human with a Heroku account.

## Applied 2026-10-12
- Token output now real (Token created/Label/Env/Store lines); examples and table use the 4-segment env form with a note that env-less tokens give 3 segments; `/metrics` -> `metrics.log`; "sub-app" -> service; reissue -> revoke and mint a new one; "4xx parse error" -> 404; "single namespace" now says existing namespace (decision 7).
- Left: "talk to us" (no contact path), 1000 events/sec cap (a rate, not a plan limit), all Heroku-side claims.
