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

clipboard — copy a short, shareable link to a /bin script

## SYNOPSIS

```
clipboard /bin/<name>
clipboard /bin/<name> --range=<spec>
clipboard /bin/<name> -- <script-args>...
clipboard /bin/<name> --range=<spec> -- <script-args>...
```

## DESCRIPTION

`clipboard` records the current shell's view of a `/bin/` script and
pushes a short link to it (`https://w3ft.net/s/k3Fz9aQ2xB`; the host depends on your deployment) onto the
system clipboard. A teammate who opens the link — signed in to the
same org — lands in the web shell with the script invocation
pre-filled, ready to run with a single Enter. The script must already
exist under `/bin`. Anyone outside the org
gets "that link doesn't exist".

Weft stores, behind the short name:

- the script path (`/bin/<name>`),
- the time range used for log queries,
- any positional or flag args you typed after `--`.

Sharing the same script, range and args again gives the same link.

The recipient sees the script invocation in their prompt, can edit
it, and presses Enter to run it. Scripts resolve to the latest
version when the recipient runs them.

## RANGE

Without `--range`, `clipboard` captures the shell's current range
(the window `range` shows). To override, pass `--range=<spec>` using
the same syntax as the `range` command — `2h..`, `1d..`,
`2026-04-12..2026-04-13`, etc. See `man range`.

## SCRIPT ARGS

Everything after a `--` separator becomes the script's arguments,
joined with single spaces. Other whitespace and shell quoting from
the original invocation are not preserved.

## ERRORS

`clipboard` fails with `no such script /bin/<name>` if the script does
not exist, `first argument must be a /bin/<name> path` for any other
first argument, and `invalid range` for a bad `--range`.

## CLIPBOARD MECHANISM

The command emits an OSC-52 escape sequence containing the link.
Modern terminal emulators (iTerm2, kitty, WezTerm, xterm with
`OSC52` enabled, the weft web shell) intercept it and write to the
system clipboard. Terminals without OSC-52 support ignore the
sequence. The link is also printed to the screen so you can select
and copy it manually.

Over SSH, OSC-52 works: the escape sequence travels with the
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

## NOT SUPPORTED

- Anything but a `/bin/<name>` script: crons, dashboards and unsaved
  pipelines can't be shared this way. Save the pipeline as a script first.
- Reading from a pipe. `clipboard` ignores standard input.
- Pinning a link to a past version of the script (`--version=`): a link
  always opens the script's current version.
- `--range <spec>` with a space. Write `--range=<spec>`.

Other options are rejected with `clipboard: unknown option <flag>`.

## SEE ALSO

range, git
