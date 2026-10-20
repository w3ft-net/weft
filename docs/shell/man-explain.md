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

explain — show how much of a pipeline is optimized

## SYNOPSIS

```
explain <pipeline>
```

## DESCRIPTION

explain takes a one-liner, exactly as you would run it, and says which of its
stages are optimized and what would make more of them so. It does not run the
pipeline, reads no logs, and is not limited by `range`.

A pipeline is fastest when its stages are part of the **optimized
retrieval**: they narrow and reshape rows as the logs are fetched, so only the
result comes back to you. Any other stage runs **post-retrieval**, on every row
that came back. Each stage is reported as one of:

```
optimized retrieval     part of the retrieval; only its result comes back
partly optimized        its filter is optimized retrieval; the rest of the
                        stage runs post-retrieval
post-retrieval          runs on rows that have already come back
```

The first stage that is not entirely optimized is the **barrier**. Every stage
after it runs post-retrieval, so the useful change is nearly always at or
before the barrier. A stage that is not optimized says why, and where there is
something to change, what to try.

When every stage is optimized retrieval, explain says so and there is nothing
to gain.

Relative paths resolve against the current directory, as they do for any
command. A script in `/bin` is explained through its body.

## EXAMPLES

```
$ explain cat /log/acme/api/main.log | awk '$3 == "ingest-stream:" { printf "%s\n", $0 }' | grep 'first row'
  1  awk '$3 == "ingest-stream:" { printf "%s\n", $0 }'  partly optimized
       pattern: optimized; action: post-retrieval
       Action does more than print the line or its fields.
       Try: Print only, e.g. `print $1, HOST`.
  2  grep 'first row'                                    post-retrieval

Barrier: stage 1.

$ explain cat /log/acme/api/main.log | grep -c boom
  1  grep -c boom  post-retrieval
       grep optimized with -i, -v, or no flags; not -c.
       Try: Use `grep PATTERN | wc -l`: both optimized.

Barrier: stage 1.

$ explain cat /log/acme/api/main.log | grep ERROR | wc -l
Every stage: optimized retrieval.
```

In a terminal the placements are colored: green for optimized retrieval,
yellow for partly optimized, red for post-retrieval.

## SAVED CRONS

A cron runs on every fire, so saving one explains it automatically. Nothing is
printed when the whole pipeline is optimized retrieval; otherwise the save
succeeds and the same report follows it.

## NOT SUPPORTED

- Running the pipeline. `explain` shows no rows, timings or row counts; use
  the pipeline itself for those.
- Options (`explain -v`, `explain --json`). They are rejected with
  `unknown option`.
- Anything that is not a log query. `explain echo hi` is an error.
