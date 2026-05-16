---
id: manual-scripts
title: Custom scripts
web:
  section: scripts
  category: customize
  order: 60
---

## Custom scripts in /bin

Anything you can type at the shell prompt can be saved as a named script under
`/bin/`. Scripts are available from every shell session and every cron
belonging to your account.  Create scripts with vim or with `cat - > myscript`.
Writing the file will validate the script for syntax errors and some
weft-specific semantics.

### Writing a script

The script body is just a pipeline. Positional parameters (`$1`, `$2`,
…) bind to the arguments the caller passes at invocation.

```
# /bin/count_errors
cat /log/$1 | grep ERROR | wc -l
```

Invoke it like any other command:

```
count_errors api.log              # → number of ERROR lines in api.log
count_errors disk.log | wc -l     # scripts compose into pipelines
```

We recommend keeping your scripts small and pipelined where possible -- weft optimizes these cases well -- but you can write more sophisticated scripts where needed. 

## Language support

The weft shell supports a generous subset of **bash** and **Lua**.
Scripts default to bash; declare Lua with a shebang on the first
line. 

### Bash

Effectively the full bash grammar is available:

- pipes (`|`), redirects (`>`, `>>`, `<`, `2>`), subshells (`( ... )`)
  and command groups (`{ ...; }`)
- variable and parameter expansion (`$var`, `${var}`, `$1`, `$@`,
  `${var:-default}`), command substitution (`$(...)` and backticks),
  arithmetic expansion (`$(( ... ))`)
- conditionals (`if`/`elif`/`else`/`fi`), `case`/`esac`, and loops
  (`for`, `while`, `until`)
- user-defined functions (`name() { ...; }`)
- the standard text toolkit as builtins: `grep`, `awk`, `sed`, `jq`,
  `cut`, `sort`, `uniq`, `wc`, `tee`, `tr`, `xargs`, `head`, `tail`,
  `cat`, `echo`, `less`

### Lua

You might reach for Lua if you need arithmetic, string
manipulation, JSON assembly, etc. 

```
#!/usr/bin/env lua
-- /bin/report
local name = arg[1]
local avg = weft.avg(name, "1h")
print(string.format("%s averaged %.2f over the last hour", name, avg))
```

Weft supplies you with Lua 5.1, including the standard `table`, `string`, and
`math` libraries, plus a restricted `os` (`os.clock`, `os.time`,
`os.difftime`). `dofile`, `loadfile`, `load`, and `require` are
disabled, and scripts time out after 30 seconds.

On top of the standard library, weft adds:

| Global | Purpose |
|---|---|
| `arg` | Positional arguments — `arg[1]`, `arg[2]`, … |
| `print(...)` | Write to stdout (tab-separated args, newline) |
| `eprint(...)` | Same shape, writes to stderr |
| `io.write(...)` | Stdout without trailing newline |
| `io.read("*l" \| "*a" \| "*n")` | Read a line, all of stdin, or a number |
| `lines()` | Iterator over stdin lines — `for line in lines() do ... end` |
| `json.encode(v)` / `json.decode(s)` | JSON ↔ Lua table |
| `weft.*` | Cron aggregates and predicates — see **Weft functions** |

Because Lua can read stdin with `lines()` or `io.read("*l")`, a Lua
script can act as a pipeline stage:

```
#!/usr/bin/env lua
-- /bin/extract_ids
for line in lines() do
  local obj = json.decode(line)
  if obj.user_id then print(obj.user_id) end
end
```

```
cat api.log | extract_ids | sort -u | wc -l
```

Weft cannot optimize Lua scripts in the same way it can coreutils pipelines. If
you need Lua, keep it at the end of a pipeline wherever you can for best
performance. 

## Editing a script with `vi`

`vi /bin/<name>` opens the script for editing. After `:w`, the
shell drops you into a follow-up buffer asking for an optional
commit message — `:wq` to record, `:q!` to skip. See **wit** for
the full version-control workflow, including `wit log`,
`wit show`, `wit diff`, `wit checkout`, and `wit amend`.

The five most recent past versions are kept; older saves are
discarded. Watch this if you iterate quickly — there's no
recycle bin past the cap.

