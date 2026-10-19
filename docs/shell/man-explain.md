---
id: man-explain
title: explain
shell:
  const: ManExplain
  topics:
    - "man explain"
    - "explain --help"
  banner: "EXPLAIN(1)"
---

## NAME

explain — show where each stage of a pipeline runs

## SYNOPSIS

```
explain <pipeline>
```

## DESCRIPTION

explain takes a one-liner, exactly as you would run it, and says where each
stage would run and what would let more of it run sooner. It does not run the
pipeline, reads no logs, and is not limited by `range`.

A pipeline is fastest when its stages run **at the source**, where the logs
live, so that only the result comes back to you. Each stage is reported as one
of:

```
at the source           runs where the logs live; only its result comes back
partly at the source    its filter runs at the source; the rest of the stage
                        runs on the rows that come back
after transfer          runs on rows that have already come back
```

The first stage that is not entirely at the source is the **barrier**. Every
stage after it runs on the rows it returns, so the useful change is nearly
always at or before the barrier. A stage that is not at the source says why,
and where there is something to change, what to try.

When every stage runs at the source, explain says so and there is nothing to
gain.

Relative paths resolve against the current directory, as they do for any
command. A script in `/bin` is explained through its body.

## EXAMPLES

```
$ explain cat /log/acme/api/main.log | awk '$3 == "ingest-stream:" { printf "%s\n", $0 }' | grep 'first row'
  1  awk '$3 == "ingest-stream:" { printf "%s\n", $0 }'  partly at the source
       Its filter runs at the source; the rest runs on the rows that come back.
       The action does more than print the line or its fields.
       Try: If it only prints, use print with fields or variables, such as print $1, HOST.
  2  grep 'first row'                                    after transfer
       It runs on the rows stage 1 returns.

Barrier: stage 1. Everything after it runs on the rows it returns.

$ explain cat /log/acme/api/main.log | grep ERROR | wc -l
Every stage runs at the source.
```

## SAVED CRONS

A cron runs on every fire, so saving one explains it automatically. Nothing is
printed when the whole pipeline runs at the source; otherwise the save
succeeds and the same report follows it.
