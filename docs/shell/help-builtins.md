---
id: help-builtins
title: Builtins
shell:
  const: HelpBuiltins
  topics:
    - "help builtins"
web:
  section: builtins
  category: weft-shell
  order: 56
---

Commands that exist in a Unix shell. Everything here behaves as it does in
bash/coreutils except where a note says otherwise.

## Reading logs

- `cat`, `head`, `tail` — read `/log/...` pseudofiles; `tail -f` follows. `head` is the oldest rows, `tail` the newest, within the current `range`. In a cron, omit `-n`: a line count can silently truncate the cron's time window.
- `ls [-l] [-a] [-h]` — lists `/bin`, `/cron.d`, `/etc`, `/log`, `/tmp`. `-l` shows size and mtime (a sparkline on `/cron.d`).
- `cd` — changes the virtual working directory.
- `less` — pages output; also automatic for long output (`man less`).

## Text pipeline

- `grep` — filter lines by pattern.
- `awk` — field predicates and extraction; meta-variables (`ENV`, `LEVEL`, ...) are bare globals, not `$NAME`.
- `sed` — stream substitution.
- `jq` — reads each row's JSON payload (the JSON part of a line with text around it). A program of only `select(...)` filters rows and prints the original lines; anything else prints jq's output, compact, one value per line. Meta-variables are `$level`, `$host`, `$ts`, ...; a missing field is `null`, so `select(.n < 100)` keeps rows without `n`.
- `cut`, `paste`, `sort`, `uniq`, `wc`, `rev`, `tac`, `tr`, `tee`, `xargs`.

Once a stage changes the text (`sort`, `awk '{ print ... }'`, `sed s/.../`), the next stage reads that text, as in any shell; the JSON payload stays attached only through stages that merely filter or count rows (`grep`, an awk pattern, `head`, `tail`).

## Files in /tmp

`/tmp` is the only writable scratch area. Expiry, size limits and how files cross into `/bin` are in [Scratch space](../manual/tmp.md#lifetime-and-limits).

- `>` / `>>` — redirect into `/tmp`.
- `mkdir`, `rm [-rf]`, `vi`, `touch`, `cp`, `mv` — as usual, within `/tmp`.
- `diff`, `comm` (sorted input), `join` (`-t ,`, `-1`, `-2` take a space before the value), `paste -d`, `wc -l -w -c -m -L`.

## Shell

- `which`, `type`, `history`, `clear`, `exit`.

## Partial implementations

Real commands with a reduced or weft-specific meaning.

- `git` — a subset over `/bin`, `/cron.d`, `/etc/dashboards` and `/etc/notify.yaml`: `log`, `show`, `diff`, `blame`, `restore --source`, `revert`, `commit --amend`, `status`. Other subcommands are refused. Last 5 past versions retained (`man git`).
- `adduser`, `deluser`, `groups`, `id` — manage org members and groups, not OS accounts; `adduser <email>` sends an invite. `/etc/passwd` is a read-only team roster (`man passwd`).

## Shell extensions

Commands with no Unix counterpart.

- `range [<start>..<end>]` — show or set the query window; it governs every read (`man range`).
- `chart` — render a pipeline or cron in the terminal (`man chart`).
- `explain <pipeline>` — say where each stage of a one-liner runs and what would speed it up, without running it (`man explain`).
- `alert` — fire a notification from a pipeline's final number (`man alert`).
- `clipboard` — copy a short link to a `/bin` script (`man clipboard`).
- `dashboard` — dashboard YAML grammar and tiles (`man dashboard`).

`help cron` and `help alerts` cover scheduling and notification.

> `man <name>` where a page exists.
