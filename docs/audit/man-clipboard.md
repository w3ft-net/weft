# Ledger: docs/shell/man-clipboard.md

| id | line | claim | type | black-box test | verdict | evidence |
|---|---|---|---|---|---|---|
| man-clipboard-01 | 7-8 | Reachable via `man clipboard` and `clipboard --help` | command | Run both | ⚠️ | static: corpus has keys "man clipboard" and "clipboard --help" (help_corpus_gen.go:111-112) so man works in a real shell; but live `sh -c "clipboard --help"` -> "clipboard: first argument must be a /bin/<name> path" (not help). `man` is not available under sh -c (🖥: type `man clipboard` in interactive weft) |
| man-clipboard-02 | 9 | Banner is `CLIPBOARD(1)` | output | `man clipboard` | 🖥 | static: corpus string begins "CLIPBOARD(1)    Weft Commands    CLIPBOARD(1)"; live render needs `man clipboard` in interactive shell |
| man-clipboard-03 | 14 | `clipboard` copies a short shareable link to a /bin script | command | `clipboard /bin/<name>` | ✅ | `clipboard /bin/scan` -> "copied: https://w3ft.net/s/pb32i0sBDV" |
| man-clipboard-04 | 19 | `clipboard /bin/<name>` form accepted | command | Run it | ✅ | `clipboard /bin/scan` accepted, link returned |
| man-clipboard-05 | 20 | `--range=<spec>` flag accepted | command | Run it | ✅ | `clipboard /bin/scan --range=2h..` -> link https://w3ft.net/s/ntKXQNxFiZ |
| man-clipboard-06 | 21 | `-- <script-args>...` separator accepted | command | Run it | ✅ | `clipboard /bin/scan -- a b` -> link .../s/MAvpMYI0tj |
| man-clipboard-07 | 22 | `--range` and `--` combined accepted | command | Run it | ✅ | `clipboard /bin/scan --range=2h.. -- --app=loom --since=1h` -> link .../s/eKfVrA6RSf |
| man-clipboard-08 | 27-28 | Records the current shell's view of a `/bin/` script | default | `clipboard`, open link, compare | 🖥 | link created, but verifying the stored view needs opening it in the web UI: click the printed URL in a signed-in browser (the local server returns 404 for GET /s/<token>, so it is a frontend route) and compare script/range |
| man-clipboard-09 | 28 | Link format `https://w3ft.net/s/k3Fz9aQ2xB` (host w3ft.net, path /s/, 10-char token) | output | Run command; inspect URL form and token length | ⚠️ | host w3ft.net, path /s/, 10-char alnum token all match (pb32i0sBDV is 10 chars). But the host is hardcoded prod even against localhost:8080 dev, and the link 404s on the dev API (`curl localhost:8080/s/pb32i0sBDV` -> 404) |
| man-clipboard-10 | 28-29 | Link is pushed onto system clipboard | command | Run then paste in local terminal | ⚠️ | Command prints OSC-52 `^[]52;c;<base64>^G` then "copied: <link>" (cat -v). Actual system-clipboard write is terminal-dependent: 🖥 run in iTerm2/kitty and paste |
| man-clipboard-11 | 29-31 | Teammate signed in to same org opening link lands in the web shell | UI | Open link in a second signed-in session of same org (needs-human for second account) | 🖥 | Open the printed link in a second browser signed in as bob (acme); expect web shell |
| man-clipboard-12 | 30-31 | Script invocation is pre-filled in prompt, run with a single Enter | UI | Open link own session; prompt holds command | 🖥 | Open link in own signed-in browser; check prompt is pre-filled with `scan` |
| man-clipboard-13 | 31-32 | Anyone outside the org gets "that link doesn't exist" | output | Open link in logged-out browser / other-org account; capture message (needs-human for other-org) | 🖥 | Open link in logged-out browser or as dana (globex); expect "that link doesn't exist" |
| man-clipboard-14 | 34-35 | Weft stores the data behind the short name | default | unverifiable | ∅ | Server-side storage not observable black-box (link not resolvable via local API) |
| man-clipboard-15 | 35 | Stores script path `/bin/<name>` | default | open link; script path pre-filled | 🖥 | Open link in web shell; prompt should hold script invocation |
| man-clipboard-16 | 36 | Stores time range used for log queries | default | open link; `range` shows stored window | 🖥 | Open link from `--range=2h..` (.../s/ntKXQNxFiZ) and run `range`; expect 2h window |
| man-clipboard-17 | 37-38 | Stores args typed after `--` | default | see 16 for args | 🖥 | Open .../s/eKfVrA6RSf; prompt should hold `scan --app=loom --since=1h` |
| man-clipboard-18 | 40 | Same script, range and args again gives the same link | output | Run clipboard twice; compare URLs | ✅ | `clipboard /bin/scan` run twice -> both https://w3ft.net/s/pb32i0sBDV. Different range/args gave different links (2h.. -> ntKXQNxFiZ, 24h.. -> wyecnPHDWO). Note: default-range link is stable here only because the range is the same relative 1d; not tested across time |
| man-clipboard-19 | 42-43 | Recipient can edit before Enter | UI | open link; edit prompt | 🖥 | Open link, edit text before Enter |
| man-clipboard-20 | 43 | Nothing executes automatically | UI | open link; confirm no output until Enter | 🖥 | Open link, confirm nothing runs until Enter |
| man-clipboard-21 | 43-45 | Scripts resolve at HEAD on recipient's side; no version pinning | default | Share link; edit script via vi/git; open link; new version runs | 🔒 | Needs a persistent write: share link, edit /bin/scan (`vi /bin/scan`), reopen link in browser, confirm new version runs |
| man-clipboard-22 | 49-50 | Without `--range`, captures shell's current range (window `range` shows) | default | `range 2h..; clipboard /bin/x`; open link, `range` | ⚠️ | `range` prints window "2026-10-11 08:07:48 .. 2026-10-12 08:07:48 [1d]" and `clipboard /bin/scan` succeeded; whether the stored range equals it needs 🖥 (open link, run `range`). Also `range 2h..` does not persist across separate `sh -c` calls, so the "set then clipboard" flow needs interactive shell |
| man-clipboard-23 | 50-51 | `--range` uses same syntax as `range` command | command | `clipboard /bin/x --range=2h..` | ✅ | `clipboard /bin/scan --range=2h..` accepted; `--range=bogus` -> "clipboard: invalid range: expected <start>..<end> (e.g., 2h.. or 2026-04-01..2026-04-12)" (same message as `range bogus`) |
| man-clipboard-24 | 51-52 | `2h..` valid spec | command | `range 2h..` | ✅ | `range 2h..` -> "2026-10-12 06:07:48 .. 2026-10-12 08:07:48  [2h]" |
| man-clipboard-25 | 51-52 | `1d..` valid spec | command | `range 1d..` | ✅ | `range 1d..` -> "2026-10-11 08:07:48 .. 2026-10-12 08:07:48  [1d]" (the 1d rejection suspect did not reproduce for range) |
| man-clipboard-26 | 52 | `2026-04-12..2026-04-13` valid spec | command | `range 2026-04-12..2026-04-13` | ✅ | `range 2026-04-12..2026-04-13` -> "2026-04-12 00:00:00 .. 2026-04-13 00:00:00  [1d]" |
| man-clipboard-27 | 52 | `man range` documents syntax | xref | `man range` | ⚠️ | static: "man range" exists in corpus (help_corpus_gen.go:121); `man` not runnable under sh -c. 🖥 type `man range` |
| man-clipboard-28 | 56-57 | Everything after `--` becomes script argument vector, joined with spaces | command | `clipboard /bin/x -- a b`; open link, read prompt | 🖥 | Link created for `-- a b` (.../s/MAvpMYI0tj); open it and read prompt: expect `scan a b` |
| man-clipboard-29 | 57-58 | Whitespace and quoting not preserved beyond simple space separation | command | `clipboard /bin/x -- "a b" c`; link prompt shows `a b c` | 🖥 | Link created for `-- "a b" c` (.../s/ou0qac9Cvl); open it, expect prompt `scan a b c`. Note the link differs from `-- a b` (MAvpMYI0tj vs ou0qac9Cvl), so quoting is apparently preserved in storage, contradicting "not preserved"; confirm in UI |
| man-clipboard-30 | 62 | Emits an OSC-52 escape sequence containing the link | output | Run `clipboard ... \| cat -v` to see `^[]52;` | ✅ | `clipboard /bin/scan \| cat -v` -> "^[]52;c;aHR0cHM6Ly93M2Z0Lm5ldC9zL3BiMzJpMHNCRFY=^G" (base64 of the link) |
| man-clipboard-31 | 63-64 | iTerm2, kitty, WezTerm, xterm with OSC52 enabled, and weft web shell write to clipboard | UI | Run in web shell then paste; others needs-human (terminal emulators) | 🖥 | Terminal/emulator behavior: run in iTerm2, kitty, WezTerm, web shell, then paste |
| man-clipboard-32 | 65-67 | Terminals without OSC-52 ignore sequence; link also printed to screen | output | Run `clipboard`, observe link printed | ⚠️ | Link is printed as "copied: <link>" after the escape sequence: true. But the word "copied:" is shown even if the terminal ignores OSC-52, minor misleading |
| man-clipboard-33 | 69-71 | Over SSH OSC-52 still works; local terminal does clipboard write | UI | needs-human (SSH session to a weft shell host) | 🖥 | Run over SSH from a local OSC-52 terminal and paste locally |
| man-clipboard-34 | 76 | `clipboard /bin/error-rate` link with current range, no args | command | Run | ✅ | Pattern works (`clipboard /bin/scan` -> link), but /bin/error-rate does not exist in this stack; `clipboard /bin/nope` -> "clipboard: no such script /bin/nope" [re-triaged 2026-10-12: `clipboard /bin/count_errors` -> `copied: https://w3ft.net/s/WdmXg0EvyA`; error-rate was an illustrative name.] |
| man-clipboard-35 | 79 | `clipboard /bin/error-rate -- --app=loom --since=1h` pre-fills `error-rate --app=loom --since=1h` | command | Run; open link | 🖥 | Syntax accepted (`-- --app=loom --since=1h` -> link .../s/eKfVrA6RSf); open link to see pre-fill. /bin/error-rate absent here |
| man-clipboard-36 | 79-80 | Prefilled command text drops `/bin/` prefix (shown as `error-rate ...`) | output | Open link; read prompt text | 🖥 | Open link from 35; check prompt starts `error-rate`/`scan` without /bin/ |
| man-clipboard-37 | 82-84 | `clipboard /bin/scan --range=24h..` pins 24-hour window regardless of sharer's range | command | Run, open link, `range` | 🖥 | `clipboard /bin/scan --range=24h..` -> link .../s/wyecnPHDWO accepted; open it and run `range`. Note `24h..` is accepted |
| man-clipboard-38 | 86-88 | `clipboard /bin/check --range=yesterday..today -- prod` is valid | command | Run | ✅ | `clipboard /bin/scan --range=yesterday..today -- prod` -> link .../s/SMHXLIca9Y (using /bin/scan as /bin/check absent) |
| man-clipboard-39 | 86-88 | `yesterday..today` accepted range tokens; means yesterday's window | command | `range yesterday..today`; check bounds | ✅ | `range yesterday..today` -> "2026-10-11 00:00:00 .. 2026-10-12 00:00:00 [1d]" = yesterday midnight to today midnight (yesterday's window) |
| man-clipboard-40 | 87-88 | `prod` passed as one positional arg | command | see 38 and link prompt | 🖥 | Open .../s/SMHXLIca9Y; expect prompt `scan prod` |
| man-clipboard-41 | 93 | SEE ALSO `range` | xref | `man range` | ⚠️ | static: "man range" key exists; not runnable under sh -c |
| man-clipboard-42 | 93 | SEE ALSO `git` | xref | `man git` | ⚠️ | static: "man git" key exists (ManualGit, line 127); not runnable under sh -c |
| man-clipboard-43 | 93 | SEE ALSO `vi` | xref | `man vi` | ❌ | static: no "man vi" key in help_corpus_gen.go (grep finds none); no docs/shell/man-vi.md |
| man-clipboard-44 | 93 | SEE ALSO `cat` | xref | `man cat` | ❌ | static: no "man cat" key in help_corpus_gen.go; no docs/shell/man-cat.md |
| man-clipboard-45 | 14 | Only `/bin` scripts supported (not other paths) | command | `clipboard /cron.d/x` errors | ✅ | `clipboard /cron.d/x` -> "clipboard: first argument must be a /bin/<name> path" |
| man-clipboard-46 | 19 | Nonexistent `/bin/<name>` behavior | command | `clipboard /bin/nope` observe error (not documented) | ✅ | `clipboard /bin/nope` -> "clipboard: no such script /bin/nope" (undocumented error; confirms the script must exist) |

## Style observations
- Line 56-58 has an awkwardly wrapped sentence ("joined with spaces. Whitespace and shell quoting from the original invocation are not preserved beyond ...") and uses "script's argument vector", jargon.
- Line 28 sample short link uses w3ft.net prod domain; may not match self-hosted/dev deployments.
- Example at 82 "24-hour window" uses `24h..` while others use `1d..`; fine.
- Error cases not documented (nonexistent script, non-/bin path, nonexistent range spec).
- Weak SEE ALSO: `git` and `cat` relevance unclear.
- No warp, Windows, "watch", or coming-soon mentions. "there is no version pinning today" is roadmap-ish language.

## Style observations (checked)
- Lines 56-58 awkward wrap confirmed in generated corpus (the long line "vector, joined with spaces. Whitespace and shell quoting from the original invocation are not" is not re-wrapped). Static check found no stray {{ }} or &lt;/&gt; and no broken fences in the corpus.
- Error cases (no such script, non-/bin path, bad range) now observed; none documented.

## Findings
- 01 (l.7-8): doc says `clipboard --help` shows this page. Under `sh -c` it errors "first argument must be a /bin/<name> path". Static corpus maps it, so likely fine in the interactive shell; confirm 🖥. Possibly product bug if the sh path is meant to support it.
- 09 (l.28): link is always https://w3ft.net/s/<10 chars>, even on a dev/self-hosted stack, where the API 404s on /s/<token>. Ambiguous; doc could say "(the host depends on your deployment)".
- 29 (l.57-58): doc says quoting is not preserved. `-- a b` and `-- "a b" c` produce distinct links, but `-- a b` vs `-- "a b" c` also differ in args count, so inconclusive; confirm in UI (🖥) before changing text.
- 32 (l.65-67): output prefix is "copied: <link>" even when terminal lacks OSC-52. Doc fine; wording minor.
- 34/35 (l.76-80): examples use /bin/error-rate and /bin/check, which must exist; `clipboard` on a missing script errors "no such script". Doc could note the script must exist.
- 43, 44 (l.93): SEE ALSO lists `vi` and `cat`, but `man vi` and `man cat` do not exist in the help corpus. Doc stale/unfinished. Proposed: "range, git" (or add man pages).
- Undocumented errors (46, 45, 23): proposed text: "`clipboard` fails with `no such script /bin/<name>` if the script does not exist, `first argument must be a /bin/<name> path` for other paths, and `invalid range` for a bad `--range`."
- Doc-wide suspects: 3-segment path, bare `myapp.log`, `watch`, globs: not applicable to this doc. `1d..` accepted by `range`: suspect did not reproduce.
- Side effect note: the clipboard runs created ~9 short-link rows server side (acme, alice); cannot be deleted from here.
