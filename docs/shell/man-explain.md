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
pipeline. When some stages are optimized and others are not, it runs one count
of the lines the optimized stages return, for the current `range`, and shows it
(see ROW COUNTS).

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

## ROW COUNTS

The lines that come back from the optimized retrieval are the lines every
post-retrieval stage has to read, so the count is what to weigh a barrier
against. It is printed above the first stage that is not entirely optimized:

```
  1  grep "query rpc: done"   optimized retrieval
  2  jq -r .msg               optimized retrieval
       259,662 lines retrieved with optimization
  3  awk '{ c = $4 }'         post-retrieval
```

With no optimized stage in front, it is the lines of the source itself
(`962,932 lines retrieved`).

The count is exact, as of now, and covers the pipeline's range, so it moves as
the range slides. It is a query of your own, so you must be able to read the
source, and it is cut off after a few seconds: when it can't be had explain
says `line count unavailable` and why, and the rest of the report is unchanged.
No post-retrieval stage is run to get it, and there is no count when every
stage is optimized, for a `tail -f`, or for a trace.

## EXAMPLES

```
$ explain cat /log/acme/api/main.log | awk '$3 == "ingest-stream:" { printf "%s\n", $0 }' | grep 'first row'
  1  awk '$3 == "ingest-stream:" { printf "%s\n", $0 }'  partly optimized
       pattern: optimized; action: post-retrieval
       Action does more than print the line or its fields.
       Try: Print only, e.g. `print $1, HOST`.
  2  grep 'first row'                                    post-retrieval

Barrier: stage 1.

$ explain cat /log/acme/api/main.log | grep -n boom
  1  grep -n boom  post-retrieval
       grep optimized with -i, -v, or no flags; not -n.

Barrier: stage 1.

$ explain cat /log/acme/api/main.log | grep -c ERROR
Every stage: optimized retrieval.
```

In a terminal the placements are colored: green for optimized retrieval,
yellow for partly optimized, red for post-retrieval.

## SAVED CRONS

A cron runs on every fire, so saving one explains it automatically. Nothing is
printed when the whole pipeline is optimized retrieval; otherwise the save
succeeds and the same report follows it.

## NOT SUPPORTED

- Running the pipeline. `explain` shows no rows or timings, and the only count
  is the retrieval's (see ROW COUNTS); use the pipeline itself for the rest.
- Options (`explain -v`, `explain --json`). They are rejected with
  `unknown option`.
- Anything that is not a log query. `explain echo hi` is an error.
