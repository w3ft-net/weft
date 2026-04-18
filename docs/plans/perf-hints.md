# Performance improvement hints

Surface alternative pipeline rewrites to the customer when their query runs
slowly. These are advisory: the original ran, results were returned, and we
attach a "next time, try this" suggestion. They never expose planner or
storage internals — only the rewritten pipeline and a one-line *why*.

`explain` stays an internal-only debugging tool for working on the planner.
Hints are the customer-facing equivalent: same insight, no architecture
leakage.

## Trigger

A query gets hint analysis only when:

- Wall-clock duration crosses a threshold (default 5s, per-customer override).
- The planner has at least one rewrite candidate that would have pushed at
  least one additional stage into shard SQL or eliminated a compute stage.
- Result-row count is non-trivial — we don't badger users for slow one-off
  exploratory pokes that returned nothing.

We deliberately don't show hints on fast queries even when a rewrite exists.
The cost of nagging a happy user outweighs the marginal teaching value.

## Sources of rewrite candidates

Each source is a small rule that takes `(QueryPlan, RuntimeStats)` and
returns zero or one suggestion. Rules are unit-testable in isolation.

1. **Pipeline reordering** — pushable stage stuck behind a non-pushable one.
   - `cat foo.log | sort | grep ERROR` → suggest `cat foo.log | grep ERROR | sort`
     (grep can push; sort can't; doing them in this order means the shard
     scans everything and ships it all back).

2. **Stage merging** — separate jq invocations that should be one.
   - `jq 'select(.x == 1)' | jq '.foo'` → `jq 'select(.x == 1) | .foo'`
     (the second jq becomes a projection only after the merge — the planner
     can fold `.foo` into SELECT but not across two stages).

3. **Better-shape rewrites** — same result, faster jq idiom.
   - `jq '.foo | sort_by(.x) | .[0]'` → `jq '.foo | min_by(.x)'`
   - `jq '.items | length'` → `jq '.items | length'` is fine, but
     `jq '[.items[]] | length'` → `jq '.items | length'`.

4. **Source narrowing** — customer used `cat foo.log` but their predicates
   match only recent rows. Suggest `tail -N foo.log` or a time-windowed
   source if we can prove it preserves the result.

5. **Predicate hoisting** — a `grep`/`awk`/`jq select` near the end of the
   pipeline could move earlier (and push to SQL).

## Delivery

- Hint travels in the query response payload, not stderr — clients can
  render it inline or suppress it.
- CLI shell prints it after results, dimmed:

  ```
  ... results ...

  Hint: this query took 7.2s. Try:
    cat vimgym.log | grep ERROR | sort
  (grep runs at the shard so we ship fewer rows back)
  ```
- API responses include it as a structured field so SDK consumers can
  decide UX.

## Implementation sketch

- Coordinator wraps execution with a timer + row counter.
- After the response is built, call `hints.Suggest(plan, stats)` which
  walks a registry of rule functions and returns the first match (or N if
  we want to be ambitious — start with one to avoid hint spam).
- Rules live in `hints/rules/*.go`, each one ~30 lines: pattern-match on
  `QueryPlan`, return a `Hint{rewrittenPipeline, reason}` or nil.
- Add a `hints.Suggest` integration test per rule using a baked plan.

## Out of scope (v1)

- **Auto-rewrite.** We suggest; the user runs the new query if they want.
- **ML / heuristic ranking.** Hand-written rules with explicit reasons.
- **Cross-query memory.** "You keep running this — save it as a view" is a
  separate feature (materialized views already exist as the destination).
- **Hints based on shard-internal stats.** We can use `RuntimeStats`
  (duration, rows scanned, bytes transferred) but not "your query hit cold
  shard X which had to spin up" — that's an architecture leak.

## Open questions

- One hint or many? Start with one, but if pipelines stack multiple
  inefficiencies, we may want to surface them all in priority order.
- Threshold per query type? `tail -f` follows are inherently long-running;
  the duration trigger has to filter out the streaming case.
- Do we let customers opt out? Probably yes — a workspace setting.
