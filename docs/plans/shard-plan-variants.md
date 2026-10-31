# Per-shard plans: what a keyed aggregate settled on

Status: 2026-10-31. Decided with Paul. Built for the keyed sum, max and min.

## The rule

A plan is chosen once, at the coordinator, from what each shard has said about its data (the field
facts, `field-types.md`), and **nothing is checked afterwards**. There is no run-time test that an
answer was exact, no retry, and no fallback plan. If any shard fails, the query fails.

For a keyed aggregate (`awk '{ s[$1] += $2 } ...'`, the max and min forms): if every shard in the
query has the facts the aggregate needs, they all run the shard `GROUP BY` and compute merges the
groups; if any shard lacks them, the base plan runs for the whole query. `exactPlan` is that rule.

## What it takes to be definitive

- **max and min**: `$N` typed as a finite decimal. SQL's cast and awk's comparison then agree on every
  row.
- **sum**: the shard casts each value to a `BIGINT` and adds (128 bits in DuckDB), returns the total as
  text, and compute adds the totals in arbitrary precision. Exact for any size, so there is no
  bound to check. It needs `$N#int` (a decimal integer of at most 15 digits). The only difference from
  awk is beyond 2^53, where awk's float sum rounds and this one does not.
- **count**: exact for any data, no facts.

## Why a shard without the facts cannot join

The obvious idea is for it to run the original stage and ship the output, and for compute to merge
that with the others' groups. There is nothing exact to merge: awk prints a float sum with six
significant digits, so adding printed partials changes the answer; a max over text is a string
comparison on that shard and a numeric one on the others; and nothing at plan time says whether the
shard's data would have fit. So the whole query takes the base plan. The cost is small: a shard is
typed after 1,000 conforming rows, so the shortcut is missed only while the live shard is that young,
or if a stray row made it dirty.

## Two designs that were built and removed

1. **Check and retry.** The shards also returned a count of values they could not vouch for and the
   sum of absolute values; compute failed the query as inexact (`InexactAggregateError`) if any
   value broke the facts or a sum passed 2^52, and the coordinator reran the base plan. It
   was correct and it made "exact" a property decided during the query, with a second plan to
   fall back to. Removed.
2. **Plain lines from a shard without facts.** Such a shard sent the key and the field's text per
   line, using the per-shard `sql` override the wire already has, and compute did the sum itself. It
   passed a differential test over every mix of forms, and measured slower than the group form it
   stood in for (83 ms against 22 ms with one of four shards on it; 134 ms with all four, past the
   base plan's 113 ms). Removed. Both are in the history of branch `shard-variants`.

## Where per-shard plans would still be needed

A plan keyed to arrays of shards on the wire (a stage list per group of shards) is not built, and
no query needs it yet. The numeric-filter exact plan is the one candidate: the shards that have the
facts return rows already filtered and need no awk stage, the ones that don't need it.

## Deploy

Every deploy rolls coordinator, compute, then shards. The exact plan is chosen only when every shard
reports the facts, and none does until a new-window shard has classified 1,000 rows (and `$N#int`
only from shards running this release, which roll last), so a new coordinator cannot send the
three new `GroupRender` values to an old compute. A count with a sort folded into it needs no facts,
and an old compute fails it ("unknown group order") until compute rolls.
