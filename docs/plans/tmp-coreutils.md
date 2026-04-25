# /tmp coreutils (phase 2)

**Status:** plan.

Phase 1 made `/tmp` writable. Phase 2 makes it useful: file-management
primitives (`cp`, `mv`, `touch`), multi-file analysis (`comm`, `diff`,
`join`, `paste`), and a broader `wc` than warp ships today.

The first design question is *where in the weft architecture these
ops execute*. Tmp data lives in RDS, not in DuckDB shards — that's a
different shape from every other pipeline weft runs today, so it
deserves a deliberate decision before we sketch any of the commands.

## Where tmp ops execute

Two viable paths. Compare and contrast:

### A. Route through the compute pool

Every weft pipeline today flows: planner extracts pushdown
predicates → shards stream pre-filtered rows → compute pool runs
remaining gpipe stages → results come back. To handle tmp through
this same shape, we'd:

- Teach the planner to recognise `/tmp/<x>` as a source distinct from
  `/log/<app>.log`.
- Add an "RDS source" path in compute that pulls
  `tmp_file_lines` rows for a given `(customer_id, path)` instead of
  receiving them from shards.
- For multi-file ops (`comm`, `diff`, `join`), have compute fetch
  from two RDS sources and join them in worker memory.
- Carry the existing fan-out / fan-in / parallelism machinery
  through unchanged.

**What this buys us:** uniform pipeline execution. `cat /log/api.log
| grep err` and `cat /tmp/errs | grep timeout` go through the same
pipeline-stage runner; the planner doesn't grow a special case past
"which source did this come from."

**What it costs:**

- The compute pool is engineered for shard-streamed rows: parallel
  fan-out, sentinel handling, time-range partitioning. None of that
  applies to a 512 KB file in a single RDS row set. We'd be
  borrowing the *name* of compute, not its capabilities.
- A new RDS-source step inside compute means compute workers grow
  an RDS connection pool. Today they only talk to shards over gRPC
  and don't need RDS credentials. That's a real change to the
  compute deployment surface (IAM, secrets, network policy).
- For `comm /tmp/a /tmp/b`, the compute worker fetches from RDS
  twice over the network, vs the coordinator (which already holds
  the connection) doing it once locally.
- Most multi-file ops aren't expressible as gpipe stages anyway.
  `diff` is a sequence-alignment algorithm over two whole inputs;
  there's no streaming "row at a time" shape that fits.

### B. Run on the coordinator, with whatever mix of SQL and Go fits each command

The coordinator already holds the RDS connection (it owns the store
package). Phase 1's tmp implementation already lives here — `cat
/tmp/foo` is `fs.ReadFile` → `store.ReadTmpFile` → SQL → bytes back
through the warp interpreter, never touching shards or compute. Phase
2 extends that pattern:

- **Command parses on coordinator → store method called → result
  emitted to stdout.**
- The store method picks SQL or in-memory Go on a case-by-case
  basis. SQL is preferred when the operation maps naturally
  (`comm` is a FULL OUTER JOIN, `cp` is INSERT…SELECT, `wc -l` is
  a column read). Go is preferred when the algorithm is procedural
  (`diff` is Myers; `join` with custom delimiters needs string
  splitting Postgres can't reasonably do).
- No new infrastructure: coordinator already does this for the rest
  of /tmp, /bin, /cron.d, /etc.

**What this buys us:**

- Single round-trip per command. Coordinator → RDS → coordinator →
  client.
- Per-command implementation can pick the cleanest expression
  (SQL FULL OUTER JOIN for comm, Go for diff) without wedging
  everything into a single execution shape.
- No changes to compute deployment surface; no new RDS-source code
  paths in compute workers.

**What it costs:**

- The coordinator does the work for tmp ops. At very large customer
  scale this could become a bottleneck — but the per-file 512 KB
  cap means *any individual op* finishes in single-digit
  milliseconds, and tmp ops aren't on the hot ingest path.
- The "uniform pipeline-stage runner" appeal of approach A is real
  if we squint, but tmp pipeline-stage execution will diverge from
  /log execution either way (two-file ops, RDS-shape data) so the
  uniformity is mostly cosmetic.

### Recommendation

Approach B (coordinator-direct). Two reasons:

1. The compute pool is the wrong tool here. Its raison d'être is
   parallel fan-out across shards and batch processing of shard-
   streamed rows. A 512 KB file in RDS doesn't benefit from any of
   that machinery, and routing through it imposes real costs (RDS
   creds in compute, two network hops where one would do, code
   paths that look unlike anything else in compute).
2. Approach B is what phase 1 already does for tmp reads. Phase 2
   is "more of the same" rather than a new architectural axis.

If we ever ship a /tmp file size cap > 100 MB or want to parallelise
something across files, revisit. For phase 2, route tmp ops on the
coordinator.

## Direct answers to phase-1 follow-ups

**Partition by `updated_at`, not `created_at`.** Right call. The
"editing extends life" promise in `docs/manual/tmp.md` falls out
for free if partman drops by `updated_at`: `UPDATE tmp_files SET
updated_at = NOW()` re-routes the row into a fresh partition with
no delete-and-reinsert dance, and `tmp_file_lines` (also partitioned
by `updated_at`) gets the same treatment via a single bulk
`UPDATE … WHERE file_id = ?` on touch or append. Phase 1 hasn't
shipped, so amend `030_tmp_filesystem.sql` in place rather than
landing a 031 to rewrite it.

**Tokenized lines for `join`.** Interesting, deferred. With the 512
KB per-file cap, the largest realistic join is ~5K × 5K rows —
in-memory hash join finishes in milliseconds. A `tokens TEXT[]`
column would only pay off with an index, and the join column
varies per query (different delimiters, different field positions),
so we'd either commit to one canonical tokenisation or carry the
column without an index. Reopen if a customer ships a slow-join
repro.

**`wc` flags + the "extensive pushdown" question.** Agree on both.
Add `-w`, `-c`, `-m`, `-L` to warp's `cmdWc` (warp-side change).
The original draft of this plan proposed an AST recognizer in the
coordinator that would convert `head -N /tmp/x`, `sort /tmp/x`, etc.
into single SQL statements — that's exactly the "extensive
pushdown" you're skeptical of. Pulled. The plan now uses SQL
*opportunistically* inside store methods, not as a separate
recognition layer with parity tests and a flag whitelist.

## What ships

### File management

Three warp builtins; all dispatch to store methods on the coordinator.

**`cp <src> <dst>`** — within /tmp, single transaction:
`INSERT INTO tmp_files`, then `INSERT INTO tmp_file_lines (file_id,
line_num, content, updated_at) SELECT $newID, line_num, content,
NOW() FROM tmp_file_lines WHERE file_id = $oldID`. SQL because the
shape fits. Per-file cap and quota enforced before the INSERT.
Cross-namespace copies (`/tmp/x → /bin/y`) rejected — `/bin` runs
writes through a syntax validator, copying arbitrary text into it
would route around that. The user can `cat /tmp/x > /bin/y` if they
really mean it.

**`mv <src> <dst>`** — `UPDATE tmp_files SET path = ?, updated_at =
NOW()`. Lines table untouched. Same cross-namespace prohibition.

**`touch <path>`** — `UPDATE tmp_files SET updated_at = NOW()` plus
`UPDATE tmp_file_lines SET updated_at = NOW() WHERE file_id = ?`.
Both rows slide into a fresh partition; the 48h clock resets. If
the file doesn't exist, create empty.

(There is deliberately no weft-specific `ttl` command — `ttl` isn't a
coreutil, and `ls -l /tmp` already shows mtime. The rule "files expire
48 hours after their last edit" plus a visible mtime is enough; the
manual can do the arithmetic for users who care, and `touch` is the
escape hatch for files they want to keep alive.)

### Multi-file analysis

Four warp builtins, each picking the right shape for its algorithm.

**`comm <a> <b>`** — three-column merge of sorted lines. SQL fits
naturally:

```sql
SELECT
    CASE WHEN b.content IS NULL THEN a.content END,
    CASE WHEN a.content IS NULL THEN b.content END,
    CASE WHEN a.content IS NOT NULL AND b.content IS NOT NULL
         THEN a.content END
FROM (SELECT DISTINCT content FROM tmp_file_lines WHERE file_id = $A) a
FULL OUTER JOIN
     (SELECT DISTINCT content FROM tmp_file_lines WHERE file_id = $B) b
ON a.content = b.content
ORDER BY COALESCE(a.content, b.content);
```

`-1`, `-2`, `-3` flags drop the corresponding column. Inputs must
be sorted; if they aren't, comm prints a warning to stderr and
continues (real comm's behaviour, derivable in Go from the row
ordering as we read).

**`diff <a> <b>`** — unified-diff format. Procedural algorithm
(Myers); SQL doesn't fit. Pull lines via two `SELECT content FROM
tmp_file_lines WHERE file_id = ? ORDER BY line_num` calls, run the
diff in Go via `github.com/sergi/go-diff/diffmatchpatch`, emit.

**`join <a> <b>`** — relational join on a key column. Flags `-t SEP`,
`-1 N`/`-2 N`, `-a 1`/`-a 2`. Tokenizing on a runtime-supplied
delimiter is awkward in SQL; do this in Go. Read both files via
SELECT, hash on the smaller side keyed by the join column, walk
the larger side and emit matches.

**`paste <a> <b> [...]`** — extend the existing `cmdPaste` at warp
`pkg/bash/commands.go:737` to accept file arguments alongside its
current stdin behaviour. Read each file's lines via SELECT, zip with
the delimiter (default tab; `-d` overrides). Streaming version
exists in case we ever lift the file-size cap, but for 512 KB the
read-all approach is fine.

### Broader `wc`

Warp's `cmdWc` today supports `-l`, `-w`, `-c` (worth confirming).
Add the rest of the GNU set: `-m` (Unicode characters, differs
from `-c` on multibyte input) and `-L` (longest-line length).
No-flag form prints `lines words bytes` per file, with a `total`
row at the end of multi-file invocations.

For `-l` specifically against a tmp path, the store method can
return `tmp_files.line_count` directly without scanning the lines
table. That's an O(1) optimisation living entirely inside the
store — not a recognition layer in the planner. Other `wc` flags
read content normally.

This is a generic warp improvement, not weft-specific. It happens
to ship in this plan because /tmp made the gap visible.

## What does NOT ship

- **`patch`.** Useful complement to `diff`; defer until requested.
- **`find /tmp -name …`.** Marginal value given the 48h horizon and
  per-customer scale. `ls -R /tmp | grep …` does the job.
- **External format conversions** (`column`, `expand`, `fold`).
  Out of scope for /tmp specifically; if/when we add them, they
  belong in the general bash toolkit.
- **A pushdown recognizer in the coordinator.** Per the discussion
  above. Store methods can use SQL freely; we just don't need a
  separate AST-recognition layer with dual fast/slow paths and
  parity tests.

## Tests

Mirror phase 1's structure (`store/tmp_test.go`,
`shellfs/tmp_test.go`):

- **Round trips:** `cp` then read both, `mv` then `ls`, `touch`
  on an aged file and confirm `ttl` jumps back near 48h.
- **Multi-file output bytes:** fixture-driven tests for `diff`,
  `comm`, `join`, `paste` asserting on the literal output. Same
  shape as warp's existing builtin tests.
- **Cross-namespace prohibition:** `cp /tmp/a /bin/b` produces a
  clear error and doesn't mutate /bin.
- **`wc` flag matrix:** one test per flag, plus the no-flag and
  multi-file paths.

## Rollout

Three PRs, in order:

1. **Phase 1 amendment** — change `030_tmp_filesystem.sql` to
   partition by `updated_at`, add `updated_at` to `tmp_file_lines`,
   adjust the store layer's writes accordingly. Lands as part of
   the phase 1 commit (or a fast-follow if phase 1 already merged).
2. **`cp` / `mv` / `touch`** — warp builtins + small store methods
   (`CopyTmpFile`, `RenameTmpFile`, `TouchTmpFile`).
3. **`comm` / `diff` / `join` / `paste --files` / broader `wc`** —
   warp builtins, small store helpers (`TmpComm`, `TmpReadLines`),
   manual update.

(2) and (3) can land in either order.

## Open questions

- **`touch` on a directory.** Bash `touch` on a dir updates its
  mtime. We could mirror that — `UPDATE` on the directory's metadata
  row only. Children don't slide forward; if the user wants all of
  them to, `touch /tmp/dir/*`. Default to that; confirm in review.
- **`tokens TEXT[]` schema, revisited.** Reopen if a customer ships
  a slow-join repro with concrete files.
