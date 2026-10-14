# Style review: docs/shell/man-clipboard.md
Verified: ✅ 15 · ❌ 2 · ⚠️ 9 · 🔒 1 · 🖥 18 · ∅ 1   (recounted from ledger rows)
Audience: shell only (links open in the web shell)   Words: ~400

## Pre-filled checklist
- [x] 1. Opens with what the reader accomplishes, not background
- [?] 2. Task-oriented sentence-case headings, no filler — all-caps man-page headings; "CLIPBOARD MECHANISM" is background-heavy.
- [ ] 3. Every code block is copy-pasteable and runs as written — l.76-86 use /bin/error-rate, /bin/check, /bin/scan that must exist (34).
- [x] 4. Realistic names; no real customer/account data
- [ ] 5. Commands/flags in `code`, UI elements in **bold** — l.93 SEE ALSO lists bare names; elsewhere commands use backticks.
- [?] 6. Sample output matches the verified output — doc never shows `copied: <link>` line the command prints (32).
- [x] 7. No internal vocabulary (OSC-52 is user-visible terminal behavior)
- [x] 8. Terminology: cron (not watch), pseudofile, namespace, org, token
- [ ] 9. No stale roadmap language — l.44-45 "no version pinning today" is stale-prone; no Windows.
- [ ] 10. Voice — l.56-58 awkward wrap and jargon "argument vector"; l.57-58 quoting claim is unconfirmed (29).
- [ ] 11. Cross-references — l.93 `vi`, `cat` have no man pages (43, 44); `git` relevance unclear.
- [x] 12. Renders cleanly in both targets (static check found no stray {{ }} or &lt;)
- [x] 13. Length proportionate; no duplicate of another doc

## Fixes the audit already established (doc is at fault)
| id | line | now says | should say |
|---|---|---|---|
| 43, 44 | 93 | range, git, vi, cat | "range, git" (or add man pages for vi and cat) |
| undoc | (none) | no error cases documented | "`clipboard` fails with `no such script /bin/<name>` if the script does not exist, `first argument must be a /bin/<name> path` for other paths, and `invalid range` for a bad `--range`." |
| 09 | 28 | `https://w3ft.net/s/k3Fz9aQ2xB` | add "(the host depends on your deployment)" |
| 34/35 | 76-86 | examples use /bin/error-rate, /bin/check | note the script must already exist |
| 29 | 56-58 | "joined with spaces. Whitespace and shell quoting ... not preserved" | rewrite plainly; confirm in UI first (inconclusive) |
| 44 | 44-45 | "there is no version pinning today" | "Scripts resolve to the latest version when the recipient runs them." (draft) |

## Needs your decision first (product vs doc)
- 01: `clipboard --help` under `sh -c` errors "first argument must be a /bin/<name> path". Wire `--help` in product, or drop it from topics.
- 09: link host is hardcoded w3ft.net; on dev/self-hosted stacks it 404s. Product should use the deployment host, or doc caveat.
- 32: output says "copied:" even when the terminal ignores OSC-52. Reword product to "link:", or leave.

## Notes
- Short-link rows created by audit runs remain server side (acme, alice); cannot be deleted from here.
- 🖥 items (clipboard write, range capture, link open in web shell, org-only access) need a human.
- Consider moving CLIPBOARD MECHANISM below EXAMPLES; it is reference, not task.

## Applied 2026-10-12
Changed: script must exist; host depends on deployment; new ERRORS section; 'no version pinning today' -> latest version; SEE ALSO now range, git; dropped 'argument vector'. Left: clipboard --help, hardcoded host, 'copied:' wording, section order.
