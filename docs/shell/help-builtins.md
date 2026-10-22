---
id: help-builtins
title: Commands
shell:
  const: HelpBuiltins
  topics:
    - "help builtins"
    - "man commands"
web:
  section: builtins
  category: weft-shell
  order: 56
---

The weft shell being a dialect of bash with a sandboxed and slimmed down
coreutils, a lot of it should come automatically for you. Otherwise, why else
would you be here? And a handful of commands --  `cd`, `wc`, `history`,
`clear`, `which`, `rev`, `tac`, `exit` -- are indeed indistinguishable from
their real counterparts. Most have limitations and/or differences. Of course we
have a handful of builtins that are weft's special sauce. An overview of all of
them are below. 

## Differences and limitations

Only the options listed here are supported. Any other option is rejected with
an error (`grep: unknown option -o`, exit status 2). The exception is `less`,
which accepts options and ignores them.

Where a command can write depends on the directory. `/log` is read-only. `/tmp` is open to every member. `/bin` needs the `binmgr` group, `/cron.d` needs `cronmgr`, and `/etc/dashboards` and `/etc/notify.yaml` need `configmgr` (`/etc/passwd` and `/etc/usage` are read-only for everyone). Expiry, size limits and how files cross into `/bin` are in [Scratch space](../manual/tmp.md#lifetime-and-limits).

| Command | Supported | Unsupported | Differences |
|---|---|---|---|
| `>`, `>>` | Redirect into a file | Redirecting into `/log` or `/etc/passwd` | |
| `awk` | Near complete implementation of One True AWK | `gensub`, `strftime`, `systime` | [Metavariables](../manual/metavars.md) are accessible in programs in constant casing (`LEVEL`, `HOST`, etc.)|
| `cat` | — | All options | Reads `/log` pseudofiles as well as `/tmp`. |
| `comm` | `-1 -2 -3` | Other options | |
| `cp` | — | `-r` and other options | Files only, not directories. Copying between `/tmp`, `/bin`, `/cron.d` and the `/etc` config files triggers the destination's save checks. |
| `cut` | `-d`, `-f` | `-c -b -s --complement` |  |
| `date` | `-u`, `-d STRING`, `-I`, `-R`, `+FORMAT` | Other options | |
| `diff` | `diff <a> <b>` | `-u -q -y` and other options | Prints `-` and `+` lines rather than a unified diff. |
| `find` | `[path...]`, `-name`, `-iname`, `-path`, `-type f\|d`, `-maxdepth N`, `-mindepth N` | `-exec`, `-delete`, `-size`, `-mtime`, boolean operators | |
| `git` | `log`, `show`, `diff`, `blame`, `status`, `rev-parse`, `restore` (`--source`), `checkout <rev> -- <path>`, `revert`, `commit --amend -m` | `add`, `rm`, `mv`, `stash`, `reset`, `clean`, branches, merges, rebases, `clone`, `fetch`, `pull`, `push`; flags beyond the ones each command lists in `man git` | Version history for `/bin`, `/cron.d`, `/etc/dashboards` and `/etc/notify.yaml` only. One linear branch where every save is a commit, so there is no staging. Only the last 5 past versions of each file are kept. `--author` and `--grep` take RE2 patterns. Writes (`restore`, `revert`, `commit --amend`) need the group that manages the file. See `man git`. |
| `grep` | `-i -v -c -n -E -q -l -h -H -r` | `-o -w -F -A -B -C -m -e -x -P -s` |  |
| `head` | `-n N`, `-N` | `-c` |  |
| `join` | `-t SEP`, `-1 N`, `-2 N`, `-a 1\|2` | `-v -o -e -i` | Matches rows with a hash, so input needn't be sorted (GNU `join` requires it). Rows with too few fields for the key are skipped. |
| `jq` | `-r`, `-c`, `-s` | `-R -n -e -S --arg` | [Metavariables](../manual/metavars.md) accessible in the form `$level`, `$host`, `$ts`, etc. |
| `less` | — | All options (accepted, no effect) | |
| `ls` | `-l -a -h` | `-R -t -r -S -1 -d -F` |  |
| `mkdir` | `-p` | Other options | `/tmp` only. |
| `mv` | — | All options | Same rules apply as for `cp` |
| `paste` | `-d` | `-s` |  |
| `rm` | `-r`, `-R`, `-f` | Other options | |
| `sed` | `s///`, `y///`, `d`, `p`, `a`, `G`, `N`, addresses and ranges, `-n`, `-e` | `-E`, `-s` | Line filters (`-n '/re/p'`, `'/re/d'`, and their `!` forms) are [optimized](../manual/fast-pipelines.md); `s///` and other programs run post-retrieval. |
| `sort` | `-r -n -u -k -t SEP` | `-f -h -V -s -b`, character offsets in keys (`-k1.2`) | |
| `tail` | `-n N`, `-N`, `-n +N`, `-f` | `-c`, `-F` | `-f` works on files under `/log` only and can't be redirected |
| `tee` | `-a` | Other options | |
| `touch` | — | All options | `/tmp` only. |
| `tr` | `tr SET1 SET2`, `tr -d SET`, ranges like `a-z` | `-s -c`, `[:class:]` |  |
| `type` | — | All options |  |
| `uniq` | `-c -d -u` | `-i -f` |  |
| `vi(m)` | — | — | Edits anything you may write, with the same groups as `>`. |
| `xargs` | `-n N`, `-P N`, `-I{}` | `-0 -d` |  |

## Weft's own commands

### Querying

| Command | What it does | Reference |
|---|---|---|
| `range [<start>..<end>]` | Show or set the time window every read uses. Relative specs like `1d..` are re-anchored on every query. | `man range` |
| `explain <pipeline>` | Say how much of a one-liner is optimized retrieval and what would speed it up. Runs nothing. See [Making pipelines fast](../manual/fast-pipelines.md). | `man explain` |
| `trace`, `trace --id <id>`, `trace --spans` | The recent traces, every line of one request across all your services, or a per-span rollup (of one request with `--id`, or of the whole time window without). | [Tracing](../manual/trace.md) |
| `chart` | Render a pipeline's output, or a cron's history, as a chart in the terminal. | [chart](../shell/man-chart.md) |
| `alert` | Fire a notification from a pipeline's final number. | [alert](../shell/man-alert.md), [notifications](../manual/alerts.md) |
| `weft::latest`, `weft::avg`, `weft::max`, ... | Read and reason about your own cron history from inside a pipeline. | [weft::\*](../manual/crons.md) |
| `dashboard` | Open a dashboard; the YAML grammar and tile kinds. | [dashboard](../shell/man-dashboard.md) |
| `clipboard` | Copy a short, shareable link to a `/bin` script. | `man clipboard`, `man sharing` |

See `man less` for paging and `man sharing` for sharing scripts.

### Sharing and history

| Command | What it does | Reference |
|---|---|---|

### Team

| Command | What it does | Reference |
|---|---|---|
| `adduser`, `deluser`, `groups`, `id` | Manage org members and groups, not operating-system accounts. `adduser <email>` sends an invite. `/etc/passwd` is a read-only team roster. | `man passwd`, `man teams` |

## Finding help

`help` lists the shell's topics, `help builtins` (or `man commands`) shows this page, and
`man` with no argument lists the manual. `<command> --help` shows the same
page as `man <command>`.
