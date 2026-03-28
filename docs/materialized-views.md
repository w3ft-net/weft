# Materialized Views

## Overview

Users can save a pipeline as a named materialized view. The system continuously applies that pipeline to incoming data on ingest, storing the results as a separate DuckDB table on each shard. Subsequent queries against the view skip the filter and transform stages entirely.

Materialized views are per-customer. There is no chaining — a view cannot reference another view.

## Creating a view

```bash
weft> save errors_by_user cat warp.log | awk '$LEVEL == "ERROR"' | jq '.payload | {user_id, action}'
# View "errors_by_user" created. Backfilling from existing shards...
```

The `save` command:

1. Parses the pipeline and splits it into pushdown predicates and transform stages (same as normal query execution)
2. Stores the view definition (name, pipeline AST, pushdown SQL, transform stages) in the coordinator's config
3. Transmits the definition to all active shards
4. Backfills: each shard runs the pipeline against its existing data and populates the view table
5. From this point forward, the active shard applies the pipeline to every ingest batch

## Storage

Each materialized view gets its own table on every shard:

```sql
-- For a view "errors_by_user" derived from:
--   cat warp.log | awk '$LEVEL == "ERROR"' | jq '.payload | {user_id, action}'

CREATE TABLE mv_errors_by_user (
    line_num    BIGINT,       -- reference to source row
    ts          TIMESTAMP,    -- preserved for time-range queries
    app         TEXT,          -- preserved for routing
    user_id     TEXT,          -- output of jq transform
    action      TEXT,          -- output of jq transform
);
```

The schema of the view table is derived from the pipeline's output shape. For pipelines that output raw text (e.g., `awk '{print $2, $1}'`), the view table stores a single `raw` TEXT column.

## Ingest path

When the active shard receives a batch of new rows:

1. INSERT into the main `logs` table (normal path)
2. For each active materialized view:
   a. Run the view's pushdown SQL against the new batch (not the full table — just the new rows)
   b. Pipe matching rows through the view's gpipe transform stages
   c. INSERT results into the view's table

This is real-time — view tables are updated on every ingest batch. The cost is proportional to the number of views × ingest throughput. For reasonable numbers of views (tens per customer) this is negligible.

## Querying a view

When the user references a view in the shell:

```bash
weft> cat errors_by_user.view | sort | uniq -c
```

The planner recognizes `errors_by_user.view` as a materialized view and routes the query to the view table on each shard:

```sql
SELECT * FROM mv_errors_by_user
WHERE ts BETWEEN '...' AND '...'
```

The `sort | uniq -c` stages run as normal gpipe compute on the result stream. The filter and jq transform from the original pipeline are already applied — no re-execution.

## View management

```bash
weft> views                          # list saved views
weft> views describe errors_by_user  # show pipeline, table schema, row counts
weft> views drop errors_by_user      # delete view and all materialized data
```

## Shard lifecycle interaction

- **New shard launch:** coordinator transmits all active view definitions to the new shard. The shard creates the view tables and begins materializing from its first ingest batch. No backfill needed since the shard has no historical data yet.
- **Shard failure/recovery:** replacement shard receives view definitions on launch. During re-ingestion from CloudWatch, view tables are populated alongside the main logs table.
- **Retention expiry:** view tables are destroyed with the shard.

## Open questions

- **View table indexing:** should view tables have DuckDB indexes on commonly filtered columns (ts, app)? Adds write overhead but speeds up queries against large views.
- **View superset optimization:** if a user queries raw logs with a pipeline that is a superset of an existing view, the planner could route to the view table with additional predicates instead of scanning the full logs table. E.g., view stores all ERROR rows; user queries `cat warp.log | awk '$LEVEL == "ERROR" && $APP == "vimgym"'`; planner rewrites to `SELECT * FROM mv_errors WHERE app = 'vimgym'`. This is a genuine query optimization but adds significant planner complexity.
- **View limits:** maximum number of views per customer? Each view adds write amplification on ingest. Need to cap this to prevent a customer with 100 views from degrading shard write performance.
- **Schema evolution:** if the source pipeline's output shape changes (e.g., the jq transform is updated), do we recreate the view table and backfill, or reject the change?
- **`save` syntax:** is `save <name> <pipeline>` the right UX, or should it be a separate command (`pipeline save`, `create view`, etc.)?
