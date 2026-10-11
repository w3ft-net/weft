---
id: man-clipboard
title: clipboard
shell:
  const: ManClipboard
  topics:
    - "man clipboard"
    - "clipboard --help"
  banner: "CLIPBOARD(1)"
---

## NAME

clipboard — copy a shareable deep link to a /bin script

## SYNOPSIS

```
clipboard /bin/<name>
clipboard /bin/<name> --range=<spec>
clipboard /bin/<name> -- <script-args>...
clipboard /bin/<name> --range=<spec> -- <script-args>...
```

## DESCRIPTION

`clipboard` builds a URL that reproduces the current shell's view of
a `/bin/` script and pushes it onto the system clipboard. A teammate
who opens the URL — signed in to the same org — lands in the web
shell with the script invocation pre-filled, ready to run with a
single Enter.

The URL encodes:

- the script path (`bin=/bin/<name>`),
- the time range used for log queries (`range=<spec>`),
- any positional or flag args you typed after `--` (`args=...`).

The recipient sees the script invocation in their prompt, edits if
they want, and hits Enter. Nothing executes automatically. Scripts
resolve at HEAD on the recipient's side — there is no version
pinning today.

## RANGE

Without `--range`, `clipboard` captures the shell's current range
(the window `range` shows). To override, pass `--range=<spec>` using
the same syntax as the `range` command — `2h..`, `1d..`,
`2026-04-12..2026-04-13`, etc. See `man range`.

## SCRIPT ARGS

Everything after a `--` separator becomes the script's argument
vector, joined with spaces and round-tripped through URL encoding.
Whitespace and shell quoting from the original invocation are not
preserved beyond simple space-separation.

## CLIPBOARD MECHANISM

The command emits an OSC-52 escape sequence containing the URL.
Modern terminal emulators (iTerm2, kitty, WezTerm, xterm with
`OSC52` enabled, the weft web shell) intercept it and write to the
system clipboard. Terminals without OSC-52 support ignore the
sequence — in either case the URL is also printed to the screen so
you can select and copy it manually.

Over SSH, OSC-52 still works: the escape sequence travels with the
rest of the terminal output and the *local* terminal performs the
clipboard write.

## EXAMPLES

```
clipboard /bin/error-rate
                        link with current range, no args

clipboard /bin/error-rate -- --app=loom --since=1h
                        link that pre-fills error-rate --app=loom --since=1h

clipboard /bin/scan --range=24h..
                        link that pins a 24-hour window regardless of
                        the sharer's current range

clipboard /bin/check --range=yesterday..today -- prod
                        link to /bin/check with one positional arg
                        and yesterday's window
```

## SEE ALSO

range, git, vi, cat
