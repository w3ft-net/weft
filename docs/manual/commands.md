---
id: manual-commands
title: Weft's own commands
web:
  section: commands
  category: weft-shell
  order: 57
---

Most of the shell is the Unix you know: `cat`, `grep`, `awk`, `sed`, `jq`,
`sort` and the rest behave as they do in bash (see **Builtins**). A few
commands exist only in weft, because they act on things a Unix shell doesn't
have: the query window, your crons, alerts, dashboards, your team. Each has a
reference page, shown with `man <name>` in the shell and listed below.

## Querying

| Command | What it does | Reference |
|---|---|---|
| `range [<start>..<end>]` | Show or set the time window every read uses. Relative specs like `1d..` are re-anchored on every query. | `man range` |
| `explain <pipeline>` | Say how much of a one-liner is optimized retrieval and what would speed it up. Runs nothing. | `man explain` |

### Making a pipeline faster

A pipeline is fastest when its stages are part of the **optimized retrieval**:
they narrow and reshape rows as the logs are fetched, so only the result comes
back. Any other stage runs **post-retrieval**, on every row that came back.
`explain` marks each stage green (optimized retrieval), yellow (partly
optimized) or red (post-retrieval), names the first one that isn't fully
optimized (the *barrier*), and says what to change:

```
$ explain cat /log/acme/api/main.log | grep -c boom
  1  grep -c boom  post-retrieval
       grep optimized with -i, -v, or no flags; not -c.
       Try: Use `grep PATTERN | wc -l`: both optimized.

Barrier: stage 1. Everything after it: post-retrieval.
```

When a pipeline is already entirely optimized, `explain` says so. Saving a
cron runs the same check and prints the report after the save, only when
something isn't optimized.

## Output

| Command | What it does | Reference |
|---|---|---|
| `chart` | Render a pipeline's output, or a cron's history, as a chart in the terminal. | `man chart` |
| `less` | Page output. Long output pages automatically; press `q` to return. | `man less` |

## Alerting and crons

| Command | What it does | Reference |
|---|---|---|
| `alert` | Fire a notification from a pipeline's final number. | `man alert`, `man notify` |
| `weft::latest`, `weft::avg`, `weft::max`, ... | Read and reason about your own cron history from inside a pipeline. | `man weft-funcs` |
| `dashboard` | Open a dashboard; the YAML grammar and tile kinds. | `man dashboard` |

## Sharing and history

| Command | What it does | Reference |
|---|---|---|
| `git` | Version history for `/bin`, `/cron.d` and `/etc`: `log`, `show`, `diff`, `blame`, `restore`, `revert`. | `man git` |
| `clipboard` | Copy a short, shareable link to a `/bin` script. | `man clipboard`, `man sharing` |

## Team

| Command | What it does | Reference |
|---|---|---|
| `adduser`, `deluser`, `groups`, `id` | Manage org members and groups, not operating-system accounts. `adduser <email>` sends an invite. | `man passwd`, `man teams` |

## Finding help

`help` lists the shell's topics, `help builtins` lists every command, and
`man` with no argument lists the manual. `<command> --help` shows the same
page as `man <command>`.
