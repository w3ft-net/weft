# /tmp filesystem

**Status:** plan.

Weft's shell today exposes four pseudo-namespaces — `/bin` (user scripts),
`/cron.d` (watches), `/etc` (notify config, usage), `/log` (read-only log
sources). Users can save logic and read logs, but they cannot save *results*.
Every query is ephemeral; the only way to keep a filtered row set around is to
re-run the pipeline.

`/tmp` closes that gap. It is a writable, per-customer scratch area backed by
two Postgres tables. Users can redirect pipeline output into `/tmp/foo`, edit
the file with `vi`, re-read it with `cat` or `grep`, `mkdir` subdirectories,
and `rm` things they no longer need. Files in `/tmp` are *never* executable —
the shell will not resolve a command name against `/tmp`, and the usual bash
invocation paths (`./foo`, `bash foo`) have no meaning in weft's virtual FS.

This is the last feature before weft goes public.

## User-facing surface

Everything works the way a bash user would expect:

```bash
weft> cat /log/api.log | awk '$LEVEL == "ERROR"' > /tmp/errs
weft> wc -l /tmp/errs
  14823 /tmp/errs

weft> cat /log/api.log | awk '$STATUS >= 500' >> /tmp/errs     # append
weft> grep timeout /tmp/errs | sort | uniq -c | sort -rn | head

weft> mkdir /tmp/incident-2026-04-22
weft> cat /log/api.log | grep 'req_id=abc123' > /tmp/incident-2026-04-22/trace
weft> vi /tmp/incident-2026-04-22/notes       # free-form text

weft> ls /tmp
errs  incident-2026-04-22/

weft> rm /tmp/errs
weft> rm -rf /tmp/incident-2026-04-22
```

Read paths (`cat`, `head`, `tail`, `grep`, `wc`, `awk`, `sed`, `jq <`,
`sort`, `uniq`, word-read builtins) all work via the standard warp
interpreter — weft does nothing special once the bytes are delivered.

## Storage

Two tables, both keyed by `customer_id`, both partitioned on `created_at`
for 48-hour TTL (see "Retention" below):

```sql
CREATE TABLE tmp_files (
    id              BIGINT NOT NULL DEFAULT nextval('public.tmp_files_id_seq'),
    customer_id     BIGINT NOT NULL,
    path            TEXT   NOT NULL,                -- "/tmp/foo" or "/tmp/sub/bar"
    is_dir          BOOLEAN NOT NULL DEFAULT FALSE,
    byte_size       BIGINT NOT NULL DEFAULT 0,      -- cached sum of line bytes
    line_count      BIGINT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE UNIQUE INDEX tmp_files_customer_path ON tmp_files (customer_id, path);

CREATE TABLE tmp_file_lines (
    file_id      BIGINT NOT NULL,           -- logical ref to tmp_files.id; no FK (partman pattern)
    line_num     BIGINT NOT NULL,
    content      TEXT   NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (file_id, line_num, created_at)
) PARTITION BY RANGE (created_at);

CREATE INDEX tmp_file_lines_file_line ON tmp_file_lines (file_id, line_num);
```

Notes:

- **No foreign key between the tables.** Same pattern as
  `cron_results.cron_watch_id` in migration
  `023_cron_results_partition.sql`: partman-managed tables drop
  partitions with `DROP TABLE`, which doesn't cooperate with FK cascades
  across partitioned children. Integrity is enforced by the store layer.
- **One row per line.** Keeps `head`/`tail`/line-numbered `grep -n` cheap.
  Whole-file reads use `ORDER BY line_num`.
- **`is_dir=true` rows have no `tmp_file_lines`.** `mkdir` inserts a row
  with the directory path, zero lines. Directory rows are addressable so
  `ls` can enumerate them without a string-prefix heuristic.
- **`byte_size` is denormalized.** Updated inside the same transaction as
  the line write. Lets quota checks run as a single aggregate query
  against `tmp_files`, no join.
- **Path normalization**: leading `/` stripped before storage, trailing
  `/` stripped. `/tmp/foo/` and `/tmp/foo` normalize to the same row.
- **`customer_id` has no FK to `customers`.** Same reason — partman drops
  partitions by DROP TABLE. Store-layer join on read covers the auth
  boundary.

## Store methods

New `store/tmp.go`:

```go
// Metadata
func (s *Store) GetTmpFile(customerID uint, path string) (*TmpFile, error)
func (s *Store) ListTmpFiles(customerID uint, dirPath string) ([]TmpFile, error)
func (s *Store) TmpUsageBytes(customerID uint) (int64, error)
func (s *Store) DeleteTmpFile(customerID uint, path string) error        // single file
func (s *Store) DeleteTmpSubtree(customerID uint, dirPath string) error  // rm -rf
func (s *Store) Mkdir(customerID uint, path string) error                // idempotent? no — errors if exists

// Content (whole-file)
func (s *Store) ReadTmpFile(customerID uint, path string) ([]byte, error)
func (s *Store) WriteTmpFile(customerID uint, path string, data []byte) error
func (s *Store) AppendTmpFile(customerID uint, path string, data []byte) error

// Streaming append (for pipeline sinks — see below)
func (s *Store) OpenTmpWriter(customerID uint, path string, append bool) (TmpWriter, error)
```

`WriteTmpFile` splits `data` on `\n`, replaces all `tmp_file_lines` for
the file, updates `byte_size`/`line_count`/`updated_at`. `AppendTmpFile`
computes `max(line_num)+1` and appends.

`OpenTmpWriter` returns an `io.WriteCloser` that batches line inserts
(e.g., 500 lines per transaction) and is meant to back streaming
redirects from pipelines. On `Close()` it commits the final batch and
updates the metadata row.

## shellfs changes

`shellfs/shellfs.go` dispatches on directory name in a series of
switch statements (`ReadFile:73-119`, `WriteFile:218-272`, `Stat:275-329`,
`ReadDir:332-408`, `Remove:411-424`). Each gets a new `case "tmp":`.

- **`ReadFile("/tmp/...")`** → `store.ReadTmpFile(customerID, path)`.
  Returns the concatenated line content (lines joined by `\n`, trailing
  newline preserved if present on write).
- **`WriteFile("/tmp/...", data)`** → enforce quota, then
  `store.WriteTmpFile(customerID, path, data)`. Reject if the path
  resolves under an existing `is_dir=false` file, or if any parent
  component doesn't exist *as* a directory. (Bash convention: `vi /tmp/a/b`
  fails if `/tmp/a` doesn't exist — we match that.)
- **`Remove("/tmp/...")`** → `store.DeleteTmpFile`. Directory removal
  (`rm -rf`) needs warp to pass a recursive flag; absent that, expose a
  `DeleteTmpSubtree` and decide per call (see the warp section).
- **`Stat`, `ReadDir`** → query `tmp_files` filtered by `customer_id`
  and path-prefix.

`FileLanguage("/tmp/...")` returns `""` unconditionally. The /bin
resolver at warp `exec.go:452` only searches `/bin/` by name, so
`/tmp/foo` can never be invoked as a command — the non-executability
guarantee falls out of the existing PATH resolution rules without any
new check.

A new top-level `/tmp` entry is added to the root `ReadDir` listing
(`shellfs.go:337-342`).

## Quota enforcement

Two independent limits — per-customer total, and per-file.

**Per-customer total**, tied to tier, mirroring `store.WatchLimit` /
`store.VolumeAllowance`:

```go
func TmpStorageAllowance(tier Tier) int64 {
    switch tier {
    case TierTrial:   return 500 * 1024                 // 500 KB
    case TierStarter: return 10  * 1024 * 1024          // 10 MB
    case TierPro:     return 100 * 1024 * 1024          // 100 MB
    case TierScale:   return 1   * 1024 * 1024 * 1024   // 1 GB
    }
}

const TmpMaxFileBytes = 512 * 1024 // 1/2 MB per individual file, all tiers
```

Numbers are deliberately conservative. With a 48-hour TTL and a 1/2 MB
per-file ceiling, /tmp is framed as a scratch pad for filtered result
sets and quick notes, not a general-purpose data lake. Ratcheting up
later is fine; ratcheting down after people depend on the space is not.

Both limits are enforced in the store:

1. Reject the write if `len(data) > TmpMaxFileBytes`, or if
   `existing_file_size + len(data) > TmpMaxFileBytes` for appends. For
   streaming writers (`OpenTmpWriter`), the limit is enforced as bytes
   accumulate and the writer returns a `tmpFileFull` error mid-stream;
   warp surfaces that as a pipeline error (`broken pipe` equivalent).
2. Reject the write if `TmpUsageBytes(customerID) + delta > allowance`.
   Same error shape as the watch-limit error at `shellfs.go:243-245`.

`/etc/usage` gains a `/tmp storage: X / Y  (N files)` line.

## Retention

48-hour TTL, partman + pg_cron, following the pattern in migration
`023_cron_results_partition.sql`:

```sql
-- In the same migration that creates tmp_files / tmp_file_lines:
SELECT partman.create_parent(
    p_parent_table := 'public.tmp_files',
    p_control      := 'created_at',
    p_interval     := '1 day',
    p_premake      := 2
);
UPDATE partman.part_config
   SET retention = '2 days', retention_keep_table = false
 WHERE parent_table = 'public.tmp_files';

SELECT partman.create_parent(
    p_parent_table := 'public.tmp_file_lines',
    p_control      := 'created_at',
    p_interval     := '1 day',
    p_premake      := 2
);
UPDATE partman.part_config
   SET retention = '2 days', retention_keep_table = false
 WHERE parent_table = 'public.tmp_file_lines';
```

pg_cron (already enabled per migration `002_usage_tables.sql`) calls
`partman.run_maintenance()` on its usual schedule. A tmp file created at
14:00 Monday disappears Wednesday evening when the Monday partition is
dropped — effectively 48-72 hours depending on when the file was
written within the partition window.

Two 48h-aware behaviors in the app layer:

- **Append after rollover.** If a user appends to `/tmp/foo` whose only
  rows live in a partition about to age out, they'll see the file
  shrink at partition-drop time. Document this; don't try to migrate
  rows forward.
- **Metadata/data drift.** Because we dropped the FK, a `tmp_files` row
  and its `tmp_file_lines` rows can theoretically age out of sync by up
  to one partition interval. In practice both created at the same
  transaction land in the same partition and age together. Edits
  (`vi`, `>` overwrite) replace lines with a fresh `created_at`, which
  resets their TTL clock — we document that as "editing a tmp file
  extends its life."

## The warp gap

**This is the one upstream change the plan requires.** Warp's redirect
handling at `pkg/bash/exec.go:371-411` allocates a `bytes.Buffer` for
`>`, `>>`, `&>`, and swaps the interpreter's stdout/stderr to write into
it — but the deferred cleanup at lines 365-369 only restores the
original writers. The buffered contents are dropped. The
`redirectCapture` type carries the `append` flag and target name, so the
data is there; it just never reaches `fs.WriteFile`.

The fix, in warp:

1. After the command body executes (before the deferred stdout
   restoration, or in a second defer after it), walk
   `interp.redirectBuffers` and flush each to the FS:
    - Non-append: `fs.WriteFile(cap.name, cap.buf.Bytes())`.
    - Append: via a new optional method on `bash.FS`:
       ```go
       type AppendFS interface {
           AppendFile(path string, data []byte) error
       }
       ```
      Fall back to read-modify-write on `FS` if the embedder doesn't
      implement it.
2. Fix `cmdTee` at `commands.go:760-764` to actually write each file
   argument via the interpreter's FS. Keep the current "stub in
   sandbox" behavior behind an opt-in flag so vimgym (the other warp
   consumer) isn't surprised — default becomes "write to FS if the
   interpreter has one."

The `BinScriptTransform` extension that landed for the /bin pushdown
work (see `docs/plans/script-body-query-detection.md`) is exactly the
precedent: a small, generic, weft-agnostic hook on warp that weft wires
into. Redirect-flushing is smaller than that — it's just "do the write
that the AST already told you to do."

Append via streaming writer (`OpenFile`-style) is a nice-to-have but
not required for v1. Redirected pipeline output fits in memory for any
realistic scratch use.

### mkdir and rm -rf

Warp's `bash.FS` interface at `fs.go:14-20` has no `Mkdir` or `RemoveAll`.
Two options:

**Option A (preferred): extend `bash.FS`.** Add `Mkdir(path string) error`
and `RemoveAll(path string) error` as optional interfaces
(`MkdirFS`, `RemoveAllFS`) alongside `LangFS`. Warp's `mkdir` and
`rm -rf` builtins route through them when present and return a clear
error when not. MemFS grows trivial implementations. Shellfs implements
them against the new store methods.

**Option B: carry mkdir/rmrf as weft-side builtins.** Register them with
`interp.RegisterCommand` the same way awk/sed/jq are registered
(`coordinator/coordinator.go:599-633` range). Avoids the warp change but
puts weft-specific vocabulary into the bash surface, and doesn't help
vimgym if it ever wants the same feature.

Option A is the right call for the same reason `BinScriptTransform` was:
keep the extension generic, let both embedders benefit.

## Pipeline → /tmp sink

For v1, a pipeline redirecting into /tmp works because warp buffers the
pipeline's stdout into `redirectCapture.buf` and, once warp is patched,
flushes it via `fs.WriteFile`/`AppendFile`. No new plumbing on the weft
side — the pipeline runs as usual (`coordinator.runLocal` or
direct-to-compute), its stdout accumulates in `outBuf`
(`coordinator.go:599-633`), and the warp-side flush does the persist.

That handles the common case (tens of megabytes). For large sinks later,
`OpenTmpWriter` returns an `io.WriteCloser` and warp streams each
pipeline's stdout into it — but we should not build that until we have a
user who needs it. Today the `RowSink` abstraction at
`shard/shard.go:18-21` is for shard→compute streaming, not storage
sinks; adding a storage RowSink is a separate plan.

**Explicit non-goal for v1:** redirecting `tail -f` output into /tmp
(continuous append from the live path at `coordinator/live_filter.go`).
Each row gets its own interpreter, so naïvely `tail -f /log/api.log >
/tmp/errs` would be N writes per row with no batching. Document this as
unsupported; revisit with a streaming writer if users ask.

## Multi-tenancy & permissions

- `customer_id` column on both tables, scoped identically to
  `shell_functions` / `cron_watches`. `ShellFS` already carries
  `customerID`; every /tmp call threads it through.
- Namespace permission checks (`canReadNamespace`,
  `canManageInNamespace` at `shellfs.go:122-165`) don't apply — /tmp is
  not scoped to log namespaces. Default: any authenticated user of the
  customer account can read/write /tmp. If we later need per-user
  scratch areas, give each user a subdir `/tmp/u/<user-id>/` and gate
  writes outside it. Not v1.
- Account removal: a store-level hook deletes all `tmp_files` /
  `tmp_file_lines` rows for the customer synchronously in the same
  transaction that soft-deletes the customer row. No FK cascade — see
  the "Storage" section for why.

## Non-executability

Three layers of "you can't run this":

1. Warp's command resolver at `exec.go:452` only reads `/bin/<name>` for
   PATH lookup. `/tmp/foo` can't become a command by typing its name.
2. `ShellFS.FileLanguage("/tmp/...")` returns `""`. If any future code
   path tries to execute a file by its FS language (the Lua branch at
   `exec.go:458`), /tmp short-circuits to "no language, no execution."
3. No `x` bit is ever reported. `vfsFileInfo.Mode()` at
   `shellfs.go:600` returns 0644 for all files regardless of namespace;
   this is already the behavior and we don't need to change it.

That's belt-and-suspenders enough.

## ls -al (cross-cutting)

Weft's `ls` is inherited straight from warp's `builtinLs` at
`pkg/bash/builtin.go:687-704` — no flag parsing, no long-form output.
For a scratch area to be useful, users need to see *when* a file was
written and *how big* it is. `ls -al` pays off in three places
(`/bin`, `/cron.d`, `/tmp`) because all three have `created_at` /
`updated_at` already.

**Warp change.** `builtinLs` grows a minimal flag parser (`-l`, `-a`,
`-h` for human-readable sizes, combinable as `-al`/`-alh`). Under `-l`,
it calls `e.Info()` on each `fs.DirEntry` — the method is already part
of the `fs.DirEntry` interface — and renders:

```
-rw-r--r--  214 B   2026-04-24 09:42  errs
drwxr-xr-x    -     2026-04-22 17:03  incident-2026-04-22
```

No ownership column (weft has no concept of per-file owner). Format:
`{mode} {size-or-dash} {YYYY-MM-DD HH:MM} {name}`. `-h` flips size to
`1.2K`-style.

**Shellfs change.** Today `vfsFileInfo` at `shellfs.go:593-603` returns
zero for `Size()` and `ModTime()`. Populate both:

- For **`/bin`**: `ReadDir` already calls `store.ListShellFunctions`
  which returns the `ShellFunction` rows — surface `len(fn.Source)` and
  `fn.UpdatedAt` on each `vfsDirEntry`. Plumb through to `vfsFileInfo`.
- For **`/cron.d`**: same — `store.ListCronWatches` rows already carry
  `UpdatedAt`; size can be the serialized form's byte length.
- For **`/tmp`**: `byte_size` and `updated_at` columns on `tmp_files`
  are designed for this.
- For **`/etc/notify.yaml`**: size = len of stored YAML, ModTime =
  `updated_at` on `notify_config`.
- For **`/etc/usage`**: synthetic — size 0, ModTime `time.Now()`.
  That's fine.
- For **`/log`**: pseudofiles have no meaningful size or mtime. Return
  `-` for size (rendered as a dash) and zero time. `ls -l /log` still
  works; it just shows dashes where numbers would go.

To keep this concrete, `vfsFileInfo` and `vfsDirEntry` grow `size int64`
and `mtime time.Time` fields, and every ReadDir case fills them in when
the data is cheap to get (which is always — ListShellFunctions etc.
already return the rows).

Hidden files (`-a`): weft has no dotfiles today. Wire the flag through
anyway so future dotfile-style surfaces (e.g., `/etc/.credentials`?)
work without another warp change.

## Rollout

Landing order:

1. **Warp PR**: redirect flush,
   `AppendFS`/`MkdirFS`/`RemoveAllFS` optional interfaces, `cmdTee`
   fix, `builtinLs` flag parsing. One PR against
   `github.com/graybeardcorp/warp`.
2. **Weft migration**: `tmp_files` + `tmp_file_lines` partitioned
   tables, partman config, 48h retention. Modeled on
   `023_cron_results_partition.sql`.
3. **Weft store**: `store/tmp.go` with the methods listed above, plus
   `TmpStorageAllowance` / `TmpMaxFileBytes`. Tests against the real
   Postgres test harness.
4. **Weft shellfs**: `case "tmp":` in each dispatcher, quota checks,
   `/tmp` root entry, `vfsFileInfo` size/mtime propagation for all
   namespaces.
5. **Weft shell manual**: `docs/manual/tmp.md` for the new namespace,
   and an update to existing command docs noting `ls -al` support.

(2) through (4) can be bench-tested against a local warp build before
(1) merges.

## Phase 2: /tmp-only coreutils

Once the namespace exists and can hold files, a handful of classic Unix
tools become useful. All of these operate on *pairs* of files or on
*sorted line sets* — perfect match for the tmp_file_lines shape, and
not meaningful against the streaming `/log` surface:

- **`comm /tmp/a /tmp/b`** — three-column output of lines only in A,
  only in B, and in both. Implementation: merge-scan over
  `tmp_file_lines` ordered by content.
- **`join -t, -1 1 -2 1 /tmp/a /tmp/b`** — relational join on a key
  column. Useful for correlating two saved result sets (e.g., errors
  joined against user table).
- **`diff /tmp/a /tmp/b`** — unified-diff two tmp files. Same line
  storage means we get line numbers cheaply.
- **`paste /tmp/a /tmp/b`** — column-merge two files line-by-line.
- **`cp /tmp/a /tmp/b`** — duplicate a tmp file. One `INSERT ... SELECT`.
- **`mv /tmp/a /tmp/b`** — rename. Single UPDATE on `tmp_files.path`.
- **`sort -u -o /tmp/sorted /tmp/a`** — in-place sort with output
  redirection. Already works via generic warp redirection, but worth
  calling out: because tmp lines are stored in Postgres, a large sort
  could be pushed down as `SELECT DISTINCT ... ORDER BY content` instead
  of round-tripping through the interpreter.

Open for discussion: **`wc -l`** already works via the generic read
path and doesn't need a /tmp-specific shortcut, but a `SELECT
line_count FROM tmp_files` lookup would make it O(1). Worth it? Only if
users hit slow counts on very large tmp files.

Phase 2 gets its own plan once phase 1 is in customers' hands and we
see which primitives they actually reach for.

## Open questions

- **Pipeline direct-to-compute → /tmp:** the direct-to-compute path
  (`coordinator/coordinator.go:468-501`) returns `Text` through the
  coordinator's compute pool. Redirection happens on the coordinator
  side once the final `Text` is in hand, so the existing warp flush
  covers it — but worth verifying no data is dropped at the
  shard→coordinator boundary for that path specifically.
- **First-use UX for the 48h TTL:** users will not read the manual
  before `>`-ing into /tmp. Do we print a one-liner the first time a
  user writes to /tmp (`# /tmp files expire after 48 hours`) and never
  again? Cheap to implement (track a per-user flag), and it's the sort
  of surprise that's better to tell them about up front than have them
  discover when their incident notes vanish.
- **`ls -al` on `/log`:** long-form output against a namespace with no
  meaningful per-file size or mtime renders a column of dashes. Is
  that fine, or should `-l` against `/log` show something else —
  row count for the most recent window, last ingest timestamp? Plan
  position: dashes for v1, revisit if it looks ugly in practice.
