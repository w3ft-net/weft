---
id: manual-sharing
title: Sharing scripts
web:
  section: sharing
  category: customize
  order: 62
---

## Sharing a script with a teammate

When a `/bin/` script is worth showing to someone else — a runbook
link, a teammate investigating the same incident — `clipboard` builds
a deep link that reproduces what you're looking at and copies it to
your system clipboard.

```
clipboard /bin/error-rate
copied: https://w3ft.net/s/k3Fz9aQ2xB
```

Paste the URL anywhere — Slack, an incident timeline, a runbook —
and a signed-in teammate who clicks it lands in the web shell with
the script invocation pre-filled. They review, hit Enter, and see
the same output you saw.

## What the link encodes

The URL is just a short name. Weft stores three things behind it,
and sharing the same script, range and args again gives the same link:

- **the script path**, always `/bin/<name>`.
- **the time window.** Defaults to your shell's current range (the
  one `range` shows). Override with `--range=<spec>`.
- **the args**, anything you typed after `--` at the `clipboard`
  invocation. Joined with spaces — there is no preservation of
  original shell quoting.

The recipient lands at the script's HEAD version. Pinning to a
specific past version isn't supported today; if a teammate needs to
see how the script behaved before someone edited it, they pair the
link with `git log -- /bin/<name>` to read the history.

## Why this and not copy-paste

Pasting a pipeline directly into Slack loses the time window, often
breaks single-quote rich content like `awk` and `jq` programs, and
makes parameterised scripts hard to share. A `/bin/` script with a
deep link sidesteps all three: the script body lives in your org's
`/bin/`, version-tracked by `git`, with the link carrying only the
runtime context (range + args), kept behind a short link.

If the thing you want to share isn't a `/bin/` script yet, save it
first:

```
cat - > /bin/check-foo
# paste the pipeline, then ctrl-d
clipboard /bin/check-foo
```

A few seconds of friction to name the artifact pays back every time
it gets re-used.

## Examples

```
clipboard /bin/error-rate
                        current range, no args

clipboard /bin/error-rate -- --app=loom --since=1h
                        pre-fills error-rate --app=loom --since=1h

clipboard /bin/scan --range=24h..
                        pins a 24-hour window regardless of your range

clipboard /bin/check --range=yesterday..today -- prod
                        yesterday's window, one positional arg
```

See `man clipboard` for the full command reference.

## What's not shared

The link does not grant access, and it only opens for members of
your org — anyone else, signed in or not, gets "that link doesn't
exist". The recipient still has to be signed in to the same org and have the namespace permissions they'd
normally need to run the script. Pasting a link into a public
channel is safe in the sense that it leaks no log data — only the
script invocation an authorized teammate could already construct.
