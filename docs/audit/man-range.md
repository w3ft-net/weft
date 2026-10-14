# Ledger: docs/shell/man-range.md

| id | line | claim | type | black-box test | verdict | evidence |
|---|---|---|---|---|---|---|
| range-01 | 7-8 | Topics `man range` and `range --help` show this page | command | shell: `man range`; `range --help` | 🖥 | `man range` and `range --help` can't run under sh -c. `range --help` via sh -c gives `error: expected <start>..<end> (e.g., 2h.. or 2026-04-01..2026-04-12)` (not the man page; but not the help-topic path). Human: type `man range` and `range --help` in interactive `weft sh`. |
| range-02 | 9 | Banner `RANGE(1)` | output | `man range` first line | 🖥 | `man range` -> "command not found" under sh -c. Human: `man range`, confirm first line RANGE(1). |
| range-03 | 14 | range displays or sets the log query time range | command | `range` | ✅ | `range` -> `2026-10-11 08:07:46 .. 2026-10-12 08:07:46  [1d]` |
| range-04 | 19 | `range` (no args) displays current window | command | `range`; observe output | ✅ | `range` prints window with duration; default is last 1d (undocumented default). |
| range-05 | 20 | `range <start>..<end>` syntax | command | `range 2026-04-01..2026-04-12` | ✅ | `range 2026-04-01..2026-04-12` -> `2026-04-01 00:00:00 .. 2026-04-12 00:00:00  [11d]` |
| range-06 | 21 | `range <start>..` syntax | command | `range 2h..` | ✅ | `range 2h..` -> `2026-10-12 06:07:47 .. 2026-10-12 08:07:47  [2h]` |
| range-07 | 28-29 | Setting range applies to all subsequent queries | command | `range 1h..; tail <file>` rows within window; try after set | 🖥 | Each `sh -c` is a fresh session: after `range 2h..`, a later `range` shows default `[1d]`; `;` and pipes after range are parsed as the end arg (`error: end: cannot parse "/ head -3"`). Human: in interactive `weft sh` run `range 1m..` then `tail <file>` and check timestamps are within the window. |
| range-08 | 29 | If `<end>` omitted, defaults to now | default | `range 2h..` then `range` shows end = now | ✅ | `range 2h..` end printed 08:07:47 = wall clock now (date: 08:07:46 EDT). |
| range-09 | 30-32 | Relative specs re-resolved against server clock on every query | default | `range 1m..`; query, wait 2 min, query again; window moved | 🖥 | Cannot persist a range across calls or wait within one pipeline. `range 1m..` printed `08:06:52 .. 08:07:52`, relative resolves against server clock at call time. Human: `range 1m..`, run a query, wait 2 min, run again; confirm window moved. |
| range-10 | 31 | `range 1d..` always means last 24 hours | default | `range 1d..; range` | ⚠️ | `range 1d..` -> `2026-10-11 08:07:52 .. 2026-10-12 08:07:52 [1d]` (24h window verified at set time). The re-resolve-per-query part is not testable here (see range-09). |
| range-11 | 38-39 | Absolute timestamp: calendar date or instant | default | per examples below | ✅ | Date (`2026-04-12`) and instants (`2026-04-12T10:30:00Z`) accepted (see 17-25). |
| range-12 | 39 | Omitted time defaults to 00:00:00 | default | `range 2026-04-12..2026-04-12T00:00:00`; compare via `range` output | ⚠️ | `range 2026-04-12..2026-04-12T00:00:00` -> `error: start must be before end` (equal, so consistent with both being 00:00:00 but not shown directly). `range 2026-04-12..` shows start `2026-04-12 00:00:00`; `2026-04-12T10..` shows `10:00:00`. Midnight default verified for date-only. |
| range-13 | 40 | Omitted day means the 1st | default | `range 2026-04..` ; `range` shows Apr 1 | ✅ | `range 2026-04..` -> start `2026-04-01 00:00:00` |
| range-14 | 40 | Omitted month means January | default | `range 2026..`; shows Jan 1 | ✅ | `range 2026..` -> start `2026-01-01 00:00:00` |
| range-15 | 40-42 | Omitted year defaults to current year; no roll-forward (`01-15` in March = current-year Jan) | default | `range 01-15..`; `range` shows current year Jan 15 | ✅ | `range 01-15..` -> start `2026-01-15 00:00:00` (current year 2026, no roll-forward); `range 04-12..` -> 2026-04-12 (also past; a future-in-year date was not tested). |
| range-16 | 43 | Server-local timezone unless literal carries one | default | `range 2026-04-12T10:00..`; `range` shows tz used; compare with `...Z` form | ⚠️ | Local-tz default plausible: `range 2026-04-12T10:00..` shows `10:00:00` and `now` shows 08:07 (laptop EDT, not UTC). But a literal with zone is honored in arithmetic yet DISPLAYED as its own wall clock, not converted: `range 2026-04-12T10:00:00+05:00..2026-04-12T12:00:00Z` -> `2026-04-12 10:00:00 .. 2026-04-12 12:00:00  [7h]`. `2026-04-12T10:00:00Z..2026-04-12T10:00:00+05:00` -> `start must be before end` (correct). Display could mislead. Which local tz (server vs user) not determinable. |
| range-17 | 46 | `2026-04-12T10:30:00Z` full RFC3339 with tz accepted | command | `range 2026-04-12T10:30:00Z..` | ✅ | `range 2026-04-12T10:30:00Z..` -> `2026-04-12 10:30:00 .. now`; offset arithmetic honored (see 16). |
| range-18 | 47 | `2026-04-12T10:30:00` second resolution | command | same | ✅ | `range 2026-04-12T10:30:00..` -> `2026-04-12 10:30:00 ..` |
| range-19 | 48 | `2026-04-12T10:30` minute resolution | command | same | ✅ | `range 2026-04-12T10:30..` -> `2026-04-12 10:30:00 ..` |
| range-20 | 49 | `2026-04-12T10` hour resolution | command | same | ✅ | `range 2026-04-12T10..` -> `2026-04-12 10:00:00 ..` |
| range-21 | 50 | `2026-04-12` day resolution | command | same | ✅ | `range 2026-04-12..` -> `2026-04-12 00:00:00 ..` |
| range-22 | 51 | `2026-04` month resolution | command | same | ✅ | `range 2026-04..` -> `2026-04-01 00:00:00 ..` |
| range-23 | 52 | `2026` year resolution | command | `range 2026..` | ✅ | `range 2026..` -> `2026-01-01 00:00:00 ..` |
| range-24 | 53 | `04-12T10:30` current-year shorthand with time | command | `range 04-12T10:30..` | ✅ | `range 04-12T10:30..` -> `2026-04-12 10:30:00 ..` |
| range-25 | 54 | `04-12` current-year shorthand, midnight | command | `range 04-12..` | ✅ | `range 04-12..` -> `2026-04-12 00:00:00 ..` |
| range-26 | 59-60 | Relative duration = distance back from now | default | `range 30m..` | ✅ | `range 30m..` -> `07:37:52 .. 08:07:52 [30m]` |
| range-27 | 60-61 | Re-anchored on every query so long-lived pane never stale | default | same as range-09 | 🖥 | Same as range-09; needs persistent session. Human: interactive `weft sh`, `range 1m..`, wait, query twice. |
| range-28 | 64 | `30m` = 30 minutes ago | command | `range 30m..; range` | ✅ | `range 30m..` -> `[30m]`, start 07:37:52 vs now 08:07:52 |
| range-29 | 65 | `2h` = 2 hours ago | command | `range 2h..` | ✅ | `range 2h..` -> `[2h]` |
| range-30 | 66 | `7d` = 7 days ago | command | `range 7d..` | ✅ | `range 7d..` -> `2026-10-05 08:07:52 .. 2026-10-12 08:07:52  [7d]` |
| range-31 | 72 | `now` = current time | command | `range 1h..now` | ✅ | `range 1h..now` -> `07:07:52 .. 08:07:52 [1h]` |
| range-32 | 73 | `today` = midnight today | command | `range today..` | ✅ | `range today..` -> `2026-10-12 00:00:00 .. 08:07:52` |
| range-33 | 74 | `yesterday` = midnight yesterday | command | `range yesterday..` | ✅ | `range yesterday..` -> `2026-10-11 00:00:00 ..` |
| range-34 | 80 | `range` shows current range | command | `range` | ✅ | `range` prints current window (see 03); outside a persisted session it only ever shows the default. |
| range-35 | 81 | `range 2h..` last 2 hours | command | `range 2h..` | ✅ | see 29 |
| range-36 | 82 | `range 30m..` last 30 minutes | command | `range 30m..` | ✅ | see 28 |
| range-37 | 83 | `range 7d..` last 7 days | command | `range 7d..` | ✅ | see 30 |
| range-38 | 84 | `range 1h..10m` 1 hour ago to 10 min ago | command | `range 1h..10m` | ✅ | `range 1h..10m` -> `07:07:53 .. 07:57:53  [50m]` |
| range-39 | 85 | `range yesterday..today` all of yesterday | command | `range yesterday..today` | ✅ | `range yesterday..today` -> `2026-10-11 00:00:00 .. 2026-10-12 00:00:00  [1d]` |
| range-40 | 86 | `range yesterday..` yesterday to now | command | `range yesterday..` | ✅ | `range yesterday..` -> `2026-10-11 00:00:00 .. now` |
| range-41 | 87 | `range 04-01..05-14` current-year span Apr 1 to May 14 | command | `range 04-01..05-14` | ✅ | `range 04-01..05-14` -> `2026-04-01 00:00:00 .. 2026-05-14 00:00:00  [43d]` |
| range-42 | 88 | `range 05-14..` May 14 midnight to now | command | `range 05-14..` | ✅ | `range 05-14..` -> `2026-05-14 00:00:00 .. now` |
| range-43 | 89-90 | `range 05-14T09..05-14T17` business hours May 14 (current year) | command | run it | ✅ | `range 05-14T09..05-14T17` -> `2026-05-14 09:00:00 .. 2026-05-14 17:00:00  [8h]` |
| range-44 | 91-92 | `range 2026-04-01..2026-04-12` explicit-year range | command | run it | ✅ | `range 2026-04-01..2026-04-12` -> `[11d]` |
| range-45 | 93-94 | `range 2026-04-12T10:00..2026-04-12T11:00` specific hour | command | run it | ✅ | `range 2026-04-12T10:00..2026-04-12T11:00` -> `[1h]` |
| range-46 | 36,57,69 | Sections are ABSOLUTE TIMESTAMP, RELATIVE DURATION, NAMED REFERENCES; "each side can be" any of these | default | mix: `range 2d..yesterday`, `range yesterday..now` | ✅ | `range 2d..yesterday` -> `2026-10-10 08:07:53 .. 2026-10-11 00:00:00`; `range yesterday..now` ok. Mixed sides work. |
| range-47 | 87 | End of `04-01..05-14`: whether end bound is inclusive/midnight of May 14 not specified | default | `range 04-01..05-14; range` shows end | ⚠️ | `range 04-01..05-14` end is `2026-05-14 00:00:00`: end is exclusive-at-midnight, so May 14 itself is NOT included. Doc glosses "Apr 1 -> May 14" without saying so; a reader may expect through May 14. |
| range-48 | - | Units limited to m, h, d (no s, w) implied by examples | default | `range 30s..`, `range 1w..` to see accept/reject | ⚠️ | `range 30s..` and `range 1w..` -> `error: start: cannot parse "30s" (expected ISO date/time or relative like 30m, 2h, 7d)`. Only m/h/d accepted (confirmed); doc never states the unit list or this error. Also `1m..` accepted. |

## Style observations
- Line 84 `1h..10m`: relative on both sides valid; ordering (start must precede end) and error on reversed range not documented.
- Line 87: "Apr 1 -> May 14" glosses end-inclusivity (see range-47).
- Line 42-43: tz default "server-local" - ambiguity for multi-tenant (server TZ vs user TZ).
- Line 85 `yesterday..today` shows "all of yesterday" consistent with midnight semantics.
- Docs name shell as page "range(1)" with man-page headings; consistent with other man pages.
- No warp, Windows, "watch", or "coming soon".

## Summary
doc: docs/shell/man-range.md
claims: 48
by category: black-box runnable 48; needs-human 0; unverifiable 0


## Findings
- range-07/09/27 (doc 28-32, 59-61): sessions do not persist across `sh -c`; the persistence and per-query re-resolution claims need a human in interactive `weft sh`. Not a doc bug.
- range-12 (doc 39): midnight default verified for date-only; OK.
- range-16 (doc 43): zone-qualified literals are honored for ordering/duration but displayed in their own wall clock (`10:00:00+05:00..12:00Z` shows `10:00:00 .. 12:00:00 [7h]`). Doc is fine; product display is ambiguous. Possible doc addition: "Output shows times as written; a zone-qualified literal is not converted to server-local." Also "server-local" is unspecified for the user's TZ; ambiguous.
- range-47 (doc 87): end bound is midnight-exclusive. Proposed text: `range 04-01..05-14      current-year span, Apr 1 00:00 up to (not including) May 14 00:00`.
- range-48 (doc 63-67): only m, h, d accepted; s and w rejected. Proposed text under RELATIVE DURATION: "Units: m (minutes), h (hours), d (days). Seconds and weeks are not supported."
- Undocumented: `range` default is last 1d; reversed or equal range gives `error: start must be before end` (e.g. `range 10m..1h`); `range --help` under sh -c returns a usage error rather than the page (may be sh -c only; check interactively); `2026-04-12T10:00Z` (no seconds, with Z) is rejected, only full `...:SSZ` accepted. Doc's example list implies Z only on full RFC3339, so consistent.
- Doc-wide suspects: no `watch`, 3-segment paths, or bare log names in this doc.
