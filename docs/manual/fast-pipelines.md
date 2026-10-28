---
id: manual-fast-pipelines
title: How the weft shell works
web:
  section: fast-pipelines
  category: weft-shell
  order: 58
---

## How the weft shell works

Weft stores your logs in a well-indexed distributed log store. Your current
[`range`](../shell/man-range.md) is in your PS1 for a reason -- this store is
time-sharded and the retrieval mechanism can eliminate whole storage nodes
based on that range value. 

But for a larger timeframe for a chatty service, this is not enough. To make
your log spelunking speedy, the weft shell will:

* watch for 1-liners that read from log files and trigger a static analysis
  process
* walk the pipeline from left to right, pushing parts of AWK and jq programs,
  plus many standard bash operations, into an intermediate representation 
* recognize when it hits a pipeline stage whose semantics cannot be pushed into
  a single pass on the log store
* compile the IR into a two-part plan: **optimized retrieval** (a log store
  query), and **post-retrieval** (the remainders of the pipeline that must be
  executed in the interpreter)
* deliver that plan to the executor, which then sends rows back to your shell

As the name implies, the more of your pipeline that makes it into optimized
retrieval, the sooner you get your log lines. Depending on whether you have
structured logs in JSON or good old fashioned unstructured lines, the best way
to optimize is to lean on jq or AWK, and wherever possible, to use
[metavariables](metavars.md). However, the following commands also get special
treatment:

* `grep`
* `sed -n '/PATTERN/p'`, `sed '/PATTERN/d'`, and their `!` forms (`sed -n '/PATTERN/!p'`, `sed '/PATTERN/!d'`)
* `head [-n]` and `tail [-n]`
* `cut -d DELIM -f LIST`, where `LIST` is discrete fields or a trailing open range (i.e. `3-`)
* `wc -l`
* `sort | uniq [-c]`
* `sort | head -N`

Anything else is going to execute post-retrieval. If your 1-liners are timing
out, you might want to run `explain` for hints of what could move into
optimized retrieval.

## explain

`explain` is a builtin function that will show you which stages of your
pipeline run optimized, partly optimized, or post-retrieval. Sometimes it can
provide you with helpful hints for rearranging things to speed it up. It doesn't actually execute the 1-liner.

### Examples

This one's going to be very fast, nothing to improve:

```
$ explain cat /log/acme/api/main.log | awk 'LEVEL == "ERROR"' | grep timeout | head -20
Every stage: optimized retrieval.
```

The next one's only partly optimized, but an easy fix. The `awk` pattern is
optimized, but `printf` makes the action more than the compiler can push into
the store, so the stage as a whole is only partly optimized:

```
$ explain cat /log/acme/api/main.log | awk 'LEVEL == "ERROR" { printf "[%s] %s\n", HOST, $2 }'
  1  awk 'LEVEL == "ERROR" { printf "[%s] %s\n", HOST, $2 }'  partly optimized
       pattern: optimized; action: post-retrieval
       Action does more than print the line or its fields.
       Try: Print only, e.g. `print $1, HOST`.

Barrier: stage 1.
```

If we don't need that formatting, we can get that whole stage optimized by following the hint:

```
$ explain cat /log/acme/api/main.log | awk 'LEVEL == "ERROR" { print HOST, $2 }'
Every stage: optimized retrieval.
```

But if we really want the formatting, it's still valuable to move it into a
second stage so that the optimized stages aren't returning whole rows:

```
$ explain cat /log/acme/api/main.log | awk 'LEVEL == "ERROR" { print HOST, $2 }' | awk '{ printf "[%s] %s\n", $1, $2 }'
  1  awk 'LEVEL == "ERROR" { print HOST, $2 }'  optimized retrieval
  2  awk '{ printf "[%s] %s\n", $1, $2 }'       post-retrieval
       Action does more than print the line or its fields.
       Try: Print only, e.g. `print $1, HOST`.

Barrier: stage 2.
```

`jq` behaves the same way. Its leading `select`s are optimized and picking
fields afterwards is too, but building a string (`"\(.path) \(.ms)"`) or
adding to a value (`.path + "?"`) leaves the rest post-retrieval.

If you have commands that stop optimization before stages that otherwise could
be optimized, weft won't figure it out. For example, messing with string casing
before a line count.

```
$ explain cat /log/acme/api/main.log | grep timeout | awk 'LEVEL == "ERROR" { print toupper($0) }' | wc -l
  1  grep timeout                                  optimized retrieval
  2  awk 'LEVEL == "ERROR" { print toupper($0) }'  partly optimized
       pattern: optimized; action: post-retrieval
       Action does more than print the line or its fields.
       Try: Print only, e.g. `print $1, HOST`.
  3  wc -l                                         post-retrieval

Barrier: stage 2.
```

This is perhaps a contrived example, but if you work in the shell by
iteratively glomming more stages onto a pipeline, you very well might just
forget that you no longer need some part of it:

```
$ explain cat /log/acme/api/main.log | grep timeout | awk 'LEVEL == "ERROR"' | wc -l
Every stage: optimized retrieval.
```

A single unsupported flag on the first stage drags the whole pipeline into
post-retrieval:

```
$ explain cat /log/acme/api/main.log | grep -E 'timeout|refused' | grep -v health | wc -l
  1  grep -E 'timeout|refused'  post-retrieval
       grep optimized with -i, -v, or no flags; not -E.
  2  grep -v health             post-retrieval
  3  wc -l                      post-retrieval

Barrier: stage 1.
```

An `awk` pattern matches the same lines with the same regular expression, and
it is optimized, so swapping just the first stage fixes all three:

```
$ explain cat /log/acme/api/main.log | awk '/timeout|refused/' | grep -v health | wc -l
Every stage: optimized retrieval.
```

Some work really does need every row. This one rewrites each line to group
messages that differ only in their numbers, which can only be done in
post-retrieval computation:

```
$ explain cat /log/acme/api/main.log | sed 's/[0-9][0-9]*/N/g' | sort | uniq -c | sort -rn | head
  1  sed 's/[0-9][0-9]*/N/g'  post-retrieval
       sed optimized only as `sed -n '/re/p'`.
       Try: Matching lines: use grep. Substitution: post-retrieval, so put it after narrowing stages.
  2  sort                     post-retrieval
  3  uniq -c                  post-retrieval
  4  sort -rn                 post-retrieval
       No optimized form.
  5  head                     post-retrieval

Barrier: stage 1.
```

No rewrite of this pipeline keeps its meaning and gets faster. What you can do
is send fewer rows into it: a smaller [`range`](../shell/man-range.md), a
narrower source, or a `grep` ahead of the `sed` for the lines you care about.
