# Field-facts benchmark

`run.py record <label>` and `run.py compare <before> <after>`; see its docstring.

`queries.json` holds the controls against weft's own `weft/shard/stats.log`. Those are
JSON, so typed awk fields don't change their plans; they guard against regressions
(`compare` exits 1 if any output differs).

## Benchmark source (not yet seeded)

The queries that field facts speed up read numeric awk columns, which no weft-owned
source has. They need a text source such as an access log:

    2026-10-30T12:00:00Z GET /api/orders 200 12.5 4096 shard-3

with a numeric `$5` (latency) and `$6` (bytes), a key column, and a small share of
rows that are not numbers in `$5` (`-`, `12ms`) so the differential cases are real.
Targets once it exists, each over a fixed past range:

- `awk '$5 > 500' | wc -l` (numeric filter: today's prefilter ships every row whose
  text sorts above "500")
- `awk '{ s[$7] += $6 } END { for (k in s) print k, s[k] }'` (keyed sum)
- `awk '{ if ($5 > m[$7]) m[$7] = $5 } END { ... }'` (keyed max)

Seeding writes to the dogfood org's ingest and needs a producer token and namespace,
so it is done by hand, not by this script.
