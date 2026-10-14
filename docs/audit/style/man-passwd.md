# Style review: docs/shell/man-passwd.md
Verified: ✅ 13 · ❌ 2 · ⚠️ 5 · 🔒 26 · 🖥 1 · ∅ 2   (recounted from ledger rows)
Audience: shell only (also web Members tab for team ops)   Words: ~410

## Pre-filled checklist
- [x] 1. Opens with what the reader accomplishes, not background
- [?] 2. Task-oriented sentence-case headings, no filler — all-caps man-page headings; banner PASSWD(5) differs from the other pages' (1).
- [ ] 3. Every code block is copy-pasteable and runs as written — l.34-39 sample roster does not match observed output (12, 17).
- [x] 4. Realistic names; no real customer/account data (acme.com, dana)
- [x] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
- [ ] 6. Sample output matches the verified output — l.36 `owner ... (all — implicit)` vs observed `admin` with explicit groups; `id` output format not shown.
- [x] 7. No internal vocabulary (warp, DuckDB, shard, ModeLess, repo paths)
- [x] 8. Terminology: cron (not watch), pseudofile, namespace, org, token
- [x] 9. No stale roadmap language or unshipped-feature promises; no Windows
- [?] 10. Voice — l.42 "dashboard's Members tab" ambiguous; l.72-73 "there is no staging" fine; l.47 "three things" incomplete (18).
- [ ] 11. Cross-references — l.89 `help index` has no corpus key (47); `id` has no `man id` (48).
- [x] 12. Renders cleanly in both targets (static check found no stray {{ }} or &lt;)
- [?] 13. Length proportionate; no duplicate — heavily duplicates the team doc ("three things" vs its "exactly two families").

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| 18 | 47-50 | A group is one of three things; managers `binmgr`/`cronmgr`/`configmgr` | add `- **nsmgr** - create and manage namespaces.` (confirm meaning with feature owner) |
| 47 | 89 | help builtins, help index | "help builtins, help" unless `help index` is wired |
| 48 | 7-10 | topics omit `man id` | add `man id` to topics, or leave as-is |
| 12 | 36 | `alice@acme.com owner active (all — implicit)` | observed role is `admin` with explicit groups; needs an owner account to confirm (draft) |
| 17 | 42-43 | Owners ... never appear in a group | same; reword once owner behavior is confirmed |
| 08 | (none) | output formats not shown | show `id` -> `uid=<email> role=<r> groups=a,b` and `groups` -> `<email>: a b` |
| 46 | (none) | unknown-email error undocumented | add `no such member: <email>` (optional) |

## Needs your decision first (product vs doc)
- 12/17: no owner exists on the dev stack and admins list groups. Product matches doc for real owners, or the doc is wrong; check a prod org.
- 18: `nsmgr` appears for every admin and is undocumented. Document it, or hide it in product.
- "owner-only" (l.41, 72) vs role `admin` in the roster: unify the role vocabulary in product or doc.
- 10/14: stray `alice  member  invited  —` row (bare name, not an email) from test agents; clean up the dev roster, not the doc.

## Notes
- 26 🔒 rows (adduser/deluser/invite flows) were not run; wording unverified.
- Sample roster uses `@acme.com`; dev stack uses `@acme.test` (harmless).
- Consider linking to or merging with the team doc to stop drift.
- Doc says "three things" while the team doc says "two families"; resolve once.

## Applied 2026-10-12
- Same role/roster/nsmgr changes as team; id/groups output formats; --help and usage-error behavior; admin-only. Left: `help index` SEE ALSO, `man id` topic, banner PASSWD(5).
