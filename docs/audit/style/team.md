# Style review: docs/manual/team.md
Verified: ✅ 10 · ❌ 4 · ⚠️ 5 · 🔒 25 · 🖥 4 · ∅ 0   (48 claims)
Audience: customer-facing web+shell   Words: ~400

## Pre-filled checklist
- [x] 1. Opens with what the reader accomplishes (roster; `cat /etc/passwd`)
- [x] 2. Task-oriented sentence-case headings, no filler
- [ ] 3. Every code block is copy-pasteable and runs as written
  - L15-21 sample roster uses `owner`/`(all — implicit)` (team-04/08); L72 `groups dana@acme.com` errors for a non-member (team-38).
- [?] 4. Realistic names; no real customer/account data
  - Doc uses acme.com; dev data uses acme.test. Fine for docs.
- [x] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
- [ ] 6. Sample output matches the verified output
  - L17-21 roles are `admin`/`member` in practice; `nsmgr` group missing.
- [x] 7. No internal vocabulary (warp, DuckDB, shard, ModeLess, repo paths)
- [x] 8. Terminology: cron, namespace, org, token
- [x] 9. No stale roadmap language; no Windows
- [ ] 10. Voice: 2nd person, present tense, active; no hedging on deterministic behavior
  - L50 "Owners ... never appear in a group" contradicts L18 and observed admin rows (team-29).
- [?] 11. Cross-references name a help/man topic or title and resolve in both shell and web
  - L85 `man passwd`; ledger could not find its source file (team-46, 🖥).
- [x] 12. Renders cleanly in both targets
- [ ] 13. Length proportionate; no duplicate of another doc
  - Ledger flags near-verbatim overlap with `man passwd` (roster, groups, adduser semantics); drift risk.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| team-04/08 | 17-18 | ROLE `owner`, GROUPS `(all — implicit)` | `admin` with listed groups (once model decided) |
| team-29/28 | 50 | "Owners have implicit full access and never appear in a group." | reword once intended model is decided (admins hold explicit groups) |
| team-14/15 | 31 | "gate the three org-wide directories" | "gate the org-wide directories and namespace management"; add an `nsmgr` row |
| team-38 | 72 | `groups dana@acme.com` (no error behavior) | mention `no such member: <email>` for non-members |
| team-06 | 20 | invited row USER shown as email | observed USER is not an email in dev (minor) |

## Needs your decision first (product vs doc)
- Owner vs admin: doc says `owner` role with implicit access; product shows `admin` with explicit groups. Rename doc vocabulary, or restore owner/implicit behavior in product.
- `nsmgr`: fourth manager group is undocumented. Document it, or fold it into an existing group.
- "dashboard's Members tab" (L24) is ambiguous (web dashboard vs `dashboard` command); confirm tab exists (team-12, 🖥).

## Notes
- Prompt `$ cat /etc/passwd` (L16) differs from other docs' bare commands (team-09, 🖥).
- 25 claims are write/permission paths (adduser, deluser, group enforcement) that no read-only run could test.
- `groups`/`id` without args (self) is in the man page but not here.

## Applied 2026-10-12
- Role is `admin` (owner only as a defined prose alias); roster sample shows explicit groups; "never appear in a group" replaced; nsmgr row added; adduser/deluser/groups/id --help, usage-error and `no such member` behaviors; admin-only. Left: Members tab wording, overlap with man passwd.
