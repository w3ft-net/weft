---
id: help-index
title: Weft shell
shell:
  const: HelpIndex
  topics:
    - "help"
    - "help --help"
    - "man help"
---

> weft shell

## Topics

- `help builtins` — every command: the Unix ones and weft's own
- `help cron` — scheduled crons, result accessors, tier limits
- `help alerts` — alert conditions, notify.yaml, dispatching

## Reference pages

- `man commands` — the same page, with a pointer to each reference page
- `man` — list the manual topics (`man git`, `man crons`, `man metavars`, ...)
- `man alert` — alert grammar and options
- `man chart` — render a cron or pipeline as an in-terminal chart
- `man clipboard` — share a /bin script via a short link
- `man dashboard` — dashboard YAML grammar and tile kinds
- `man explain` — how much of a pipeline is optimized, and what would speed it up
- `man range` — show or set the query time window
- `man less` — the in-shell pager
- `man passwd` — team roster, groups, and adduser/deluser

> Tip: long output is paged automatically — press q to return.
