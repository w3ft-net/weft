# Ledger: docs/shell/man-less.md

| id | line | claim | type | black-box test | verdict | evidence |
|---|---|---|---|---|---|---|
| less-01 | 7-8 | Topics `man less` and `less --help` show this page | command | shell: `man less`; `less --help` | ❌ | `sh -c 'man less'` and `'less --help'` -> exit 0, stderr `bash: man: command not found` / `bash: less: command not found`; `./weft man less` just prints the top-level CLI usage. No man page reachable non-interactively |
| less-02 | 9 | Banner `LESS(1)` | output | shell: `man less` first line shows `LESS(1)` | ❌ | `sh -c 'man less'` -> `bash: man: command not found`; no `LESS(1)` banner (interactive path untested) |
| less-03 | 14 | `less` pages output one screen at a time | command | shell: `tail -n 500 <file> \| less` | ❌ | `sh -c 'tail -n 5 /log/acme/prod/api/main.log \| less'` -> exit 0, 5 lines printed raw to stdout, no paging (non-interactive, so a TTY pager 🖥 untestable; but output is not paged) |
| less-04 | 19 | Synopsis `<pipeline> \| less` | command | shell | ⚠️ | `tail -n 5 F \| less` accepted as final stage (exit 0, passthrough). `less` mid-pipeline: `tail -n 5 F \| less; echo after` -> `compute exit 127: bash: less: command not found`; `tail\|less\|wc -l` -> `0` (vs 5 without less); `tail\|less\|head -2` -> empty. `less F` -> `plan: pipeline must start with cat, head, or tail ... "less" can't be the first stage` |
| less-05 | 24-26 | In-process pager; unlike system less(1) no subprocess, no TTY hand-off | default | unverifiable (internal) | ∅ | internal |
| less-06 | 26-27 | Works the same in web shell as native CLI | UI | run same pipeline in web shell and `weft` CLI; compare | 🖥 | web shell needs browser; CLI non-interactive `\| less` is a passthrough noop (see less-03) |
| less-07 | 28-29 | When a pipeline ends with `\| less`, the query runs to completion before paging | default | shell: `tail -n 100000 <file> \| less`; pager appears only after complete | ⚠️ | `tail -n 5 F \| less` returns all lines immediately and exits 0; completion before paging cannot be observed without a TTY |
| less-08 | 29-30 | Result loaded into pager; shell enters ModeLess until q | UI | shell: press q returns to prompt | 🖥 | needs interactive terminal to see ModeLess / q |
| less-09 | 30 | "ModeLess" visible name (internal jargon) | xref | unverifiable | ∅ | internal jargon, not observable |
| less-10 | 32-34 | Help and man pages auto-page when output would overflow viewport | UI | shell: `man range` in small terminal / `help` pages; q returns | 🖥 | `help`/`man` are not available in `sh -c` (`bash: help: command not found`); auto-paging needs interactive terminal |
| less-11 | 38 | space, Page Down, f, Ctrl+F go forward one screen | UI | each key in pager | 🖥 | key handling in pager requires interactive terminal/browser |
| less-12 | 39 | b, Page Up, Ctrl+B back one screen | UI | each key | 🖥 | key handling in pager requires interactive terminal/browser |
| less-13 | 40 | j, Down forward one line | UI | each key | 🖥 | key handling in pager requires interactive terminal/browser |
| less-14 | 41 | k, Up back one line | UI | each key | 🖥 | key handling in pager requires interactive terminal/browser |
| less-15 | 42 | g, gg jump to top | UI | each key | 🖥 | key handling in pager requires interactive terminal/browser |
| less-16 | 43 | G jump to bottom | UI | key | 🖥 | key handling in pager requires interactive terminal/browser |
| less-17 | 44 | q, Ctrl+C exit the pager | UI | each key | 🖥 | key handling in pager requires interactive terminal/browser |
| less-18 | 50 | `/pattern` searches forward | UI | type `/ERROR` + Enter | 🖥 | key handling in pager requires interactive terminal/browser |
| less-19 | 50 | Search is case-sensitive substring (not regex) | UI | `/error` vs `/ERROR`; `/E.R` no match | 🖥 | key handling in pager requires interactive terminal/browser |
| less-20 | 51 | `n` next match | UI | key | 🖥 | key handling in pager requires interactive terminal/browser |
| less-21 | 52 | `N` previous match | UI | key | 🖥 | key handling in pager requires interactive terminal/browser |
| less-22 | 57-58 | Bottom row shows source title on left | UI | observe status row | 🖥 | key handling in pager requires interactive terminal/browser |
| less-23 | 58 | Percent position on right | UI | observe status row | 🖥 | key handling in pager requires interactive terminal/browser |
| less-24 | 58-59 | Incremental search and error messages replace status briefly | UI | type `/foo`; search nonexistent pattern, observe error | 🖥 | key handling in pager requires interactive terminal/browser |
| less-25 | 63 | SEE ALSO `help`, `man alert`, `man range` exist | xref | shell: `help`; `man alert`; `man range` | ❌ | `sh -c 'help'`, `'man alert'`, `'man range'` -> exit 0, `bash: help\|man: command not found`; non-interactively none of the SEE ALSO targets resolve (interactive path untested) |

## Style observations
- Line 18: SYNOPSIS only shows `<pipeline> | less`; no mention of `less file` or whether `less` works on pseudofile argument.
- Memory notes say `less` was "currently broken" (CLI silent noop, web shell no pager); doc asserts it works in both - high-priority to verify.
- Line 30 exposes internal name "ModeLess".
- Line 43 `g, gg`: "g" alone and "gg" both jump; slightly odd.
- Navigation uses "Page Down" etc. in a fenced block; fine.
- No warp, Windows, "watch", or "coming soon".

## Findings

- less-01/02/25 (lines 7-9, 63): doc says `man less`, `less --help`, `help`, `man alert`, `man range` show pages. Actual in `weft sh -c`: `bash: man: command not found` (exit 0). Likely help/man are interactive-shell builtins only (not testable here), so possibly 🖥 rather than a bug; recommend verifying in an interactive session. Ambiguous.
- less-03/04/07 (lines 14, 18, 28): doc says `| less` pages the result. Actual non-interactive: trailing `| less` is silently stripped and output printed raw (exit 0); mid-pipeline `less` is `command not found` (exit 127 in compute) and `| less | wc -l` silently returns 0; `less FILE` errors. Matches project notes that less is a noop. Product gap plus doc overstates. Recommend doc note: "`less` must be the last stage of a `tail`/`cat`/`head` pipeline and only pages in an interactive terminal; in `weft sh -c` it passes output through." Proposed SYNOPSIS: `tail|cat|head <path> | ... | less`.
- less-04: `| less | wc -l` returning 0 rather than an error or 5 is a product bug (silent data loss).

## Summary
doc: docs/shell/man-less.md
claims: 25
by category: black-box runnable ~22; needs-human 0; unverifiable 3 (approximate)
