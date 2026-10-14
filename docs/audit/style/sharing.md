# Style review: docs/manual/sharing.md
Verified: ✅ 11 · ❌ 0 · ⚠️ 12 · 🔒 4 · 🖥 9 · ∅ 3   (39 claims; recounted from verdict column)
Audience: customer-facing web+shell   Words: ~475

## Pre-filled checklist
- [x] 1. Opens with what the reader accomplishes — l.12-15 states the task.
- [ ] 2. Task-oriented sentence-case headings — "Why this and not copy-paste" (l.44) is rationale, not a task; consider moving after examples.
- [ ] 3. Every code block copy-pasteable — examples use `/bin/error-rate`, `/bin/check` that don't exist unless the reader has them (sharing-02/25); not stated.
- [x] 4. Realistic names — `error-rate`, `check-foo`, `w3ft.net` plausible; `--app=loom` is odd but fine.
- [x] 5. Commands/flags in `code`, UI in bold — consistent.
- [x] 6. Sample output matches — `copied: https://w3ft.net/s/<10 chars>` matches (sharing-02/04).
- [x] 7. No internal vocabulary — none (git history mention is user-facing).
- [x] 8. Terminology — org, namespace, script consistent.
- [ ] 9. No stale roadmap language — "isn't supported today" (l.40) is a stale hedge; no Windows.
- [ ] 10. Voice — l.51 "kept behind a short link" awkward; l.85-88 contradictory.
- [?] 11. Cross-references — `man clipboard` (l.81) exists, but its SEE ALSO lists vi/cat with no pages; check web resolution.
- [ ] 12. Renders cleanly — l.87 is one very long unwrapped line (hand-edit); wraps oddly in source, renders ok.
- [x] 13. Length proportionate; some overlap with `man clipboard` examples.

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| sharing-38 | 85-90 | "...anyone else, signed in or not, gets 'that link doesn't exist'. The recipient still has to be signed in..." | "The link does not grant access. It only opens for signed-in members of your org; anyone else gets \"that link doesn't exist\". Recipients also need the namespace permissions required to run the script. A link is safe to paste in a public channel: it carries only the script invocation, no log data." |
| sharing-02/25/26/29 | 17-19, 67-79 | examples use `/bin/error-rate`, `/bin/check` | add: "The script must already exist in `/bin/`." |
| sharing-31 | 35, 71-77 | `--range` position implied | "Put `--range` before `--`; anything after `--` is passed to the script as-is." |
| sharing-03 | 19 | `https://w3ft.net/s/...` | "(host depends on your deployment)" (draft) |
| sharing-11 | 33-34 | defaults to current range | stored form unconfirmed (default link differs from explicit `--range=1d..`); verify in browser |
| sharing-21 | 53-54 | "save it first" | optionally name the error: "first argument must be a /bin/<name> path" |
| style | 40 | "isn't supported today" | "Links always open the current version." (draft) |
| style | 51 | "kept behind a short link" | drop the clause |

## Needs your decision first (product vs doc)
- Link access (sharing-34/35/38): signed-out users get "doesn't exist" vs login redirect? Confirm in browser, then make the doc match the real flow.
- Default range storage (sharing-11): link appears relative (re-resolved at open) vs fixed window. State which; if fixed is intended, fix product.
- Quoting not preserved (sharing-14): `-- "a b" c` equals `-- a b c`. Documented; keep, or preserve quoting.
- Hardcoded w3ft.net host on dev/self-hosted (sharing-03): make configurable or add doc caveat.

## Notes
- 🖥 items need a browser: prefill without auto-run (sharing-05/06/07), HEAD version (-15), permission denial (-36, no non-admin user in acme).
- Consider moving "Why this and not copy-paste" below Examples, or trimming it.
- The ctrl-d step (l.56-60) is only checked in the CLI; confirm in the web shell.
- Examples block uses column-aligned glosses unlike other docs; renders fine but is unique.

## Applied 2026-10-12
Changed: script must exist + host varies; --range before --; 'isn't supported today' -> always current version; dropped 'kept behind a short link'; rewrote What's not shared (signed-in org members only). Left: signed-out behavior and default-range storage (browser), quoting, hardcoded host.
