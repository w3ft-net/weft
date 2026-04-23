---
id: help-builtins
title: Builtins
shell:
  const: HelpBuiltins
  topics:
    - "help builtins"
web:
  section: builtins
  order: 60
---

## Querying logs

- `cat /log/<app>.log` — stream all lines for an app
- `head /log/<app>.log` — first N lines (oldest)
- `tail /log/<app>.log` — last N lines; `tail -f` follows
- `apps` — list available apps

## Text pipeline

- `grep` — filter lines by pattern
- `awk` — field-oriented extraction / predicates
- `sed` — stream substitution
- `jq` — JSON filtering and projection (implies JSON source)
- `cut`, `paste` — column slicing and joining
- `sort`, `uniq` — ordering and de-duplication
- `wc` — line/word/byte counts
- `rev`, `tac` — reverse chars / reverse line order
- `tee`, `tr` — duplicate to file / translate chars
- `xargs` — build commands from stdin
- `less` — page the final output (see `man less`)

## Filesystem

- `ls [path]` — list `/bin`, `/cron.d`, `/etc`, `/log`
- `cat /bin/<fn>` — view a saved function
- `cat /cron.d/<watch>` — view a watch definition
- `rm <path>` — delete a function or watch
- `vi <path>` — edit (vim keys)
- `cd <path>` — change the virtual cwd

## Alerting

- `<pipeline> | alert <condition>` — see `help alerts` or `man alert`

## Shell

- `range` — show or set the query window
- `which <name>` — show where a command lives
- `type <name>` — identify a command
- `history` — past commands
- `clear` — clear the screen
- `exit` — quit the shell

> Each entry above is a command — `man <name>` where a page exists.
