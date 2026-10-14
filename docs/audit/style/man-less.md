# Style review: docs/shell/man-less.md
Verified: ✅ 0 · ❌ 4 · ⚠️ 2 · 🔒 0 · 🖥 17 · ∅ 2   (copy from the ledger; recount the verdict column)
Audience: customer-facing web+shell   Words: ~220

## Pre-filled checklist
Mark each: [x] looks fine, [ ] needs a change, [?] your call. Add a short note (≤ 20 words) with a line number when not [x]. This is my read; you decide.
- [x] 1. Opens with what the reader accomplishes — NAME line is the man-page convention.
- [x] 2. Task-oriented sentence-case headings — man-page uppercase sections follow the house man format.
- [ ] 3. Every code block is copy-pasteable and runs as written — L19 `<pipeline> | less` is a pass-through; mid-pipeline `less` loses data (less-04).
- [x] 4. Realistic names; no real customer/account data
- [x] 5. Commands/flags in `code`, UI elements in **bold**, used consistently
- [?] 6. Sample output matches the verified output — keys/status line all 🖥; none confirmed.
- [ ] 7. No internal vocabulary — L30 "ModeLess"; L24-26 "in-process", "subprocess", "TTY hand-off" are implementation detail.
- [x] 8. Terminology: cron, pseudofile, namespace, org, token — not applicable.
- [?] 9. Stale/unshipped promises — L24-26 asserts web and CLI parity that memory and ledger say is not true (less-06).
- [ ] 10. Voice — L32-33 and L28-30 are deterministic claims that observation contradicts.
- [ ] 11. Cross-references — L63 `help`, `man alert`, `man range` unresolved non-interactively (less-25).
- [x] 12. Renders cleanly in both targets (no stray {{ }}, raw &lt;, broken tables/fences)
- [?] 13. Length proportionate — L32-33 (auto-paging of help/man) belongs in the shell overview, not here.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|----|------|----------|------------|
| less-03/04/07 | 14, 19, 28 | `| less` pages the result | "`less` must be the last stage of a `cat`/`head`/`tail` pipeline and pages only in an interactive terminal; in `weft sh -c` it passes output through." (ledger) |
| less-09 | 30 | "the shell enters ModeLess until you press q" | "the shell shows the pager until you press q" (draft) |
| less-05 | 24-26 | "no subprocess, no TTY hand-off" | drop internals; "Runs inside the shell" (draft) |
| less-06 | 26-27 | works the same in web shell as native CLI | unverified; remove or confirm in browser (draft) |
| less-25 | 63 | `help`, `man alert`, `man range` | resolve interactively; keep if confirmed |

## Needs your decision first (product vs doc)
- less-03/04/07: pager is a silent no-op under the CLI (and web has no pager). Fix the product so the page is true, or rewrite the page and `help builtins` to say what works.
- less-04: `tail | less | wc -l` prints 0 (silent data loss). Make `less` mid-pipeline an error, or a true pass-through.
- less-01/02/25: `man`/`help` are not found under `sh -c`; likely interactive builtins. Confirm in a TTY before treating as bugs.

## Notes
- 17 of 25 claims are key-handling 🖥 rows; nothing here has been confirmed by a human yet.
- SYNOPSIS (L18) omits that `less FILE` and a non-final `less` are errors.
- L43 "g, gg" is odd; one form suffices if both truly work.
- Any change to this page should mirror `help builtins` L33.
- Memory notes `less` as broken in weft; this page is the main place the claim is made.

## Applied 2026-10-12 (docpass)
- Only change: removed internal term "ModeLess". Left everything that depends on the pager landing: the "same in web and CLI" claim, the auto-paging of help/man, SYNOPSIS (less FILE / non-final less behavior), key/status-line rows (all interactive).
