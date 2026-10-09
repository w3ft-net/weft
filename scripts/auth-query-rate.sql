-- Auth/stamp query rates, measured in one session: snapshot, sleep, snapshot.
--
--   kevcons db query --timeout 90 --file scripts/auth-query-rate.sql
--
-- Reads the cumulative pg_stat counters twice, 30 s apart (edit pg_sleep to
-- change the window; keep --timeout above it), and prints per-second rates.
-- No extensions needed.
--
-- What to look at (see docs/plans/weftd-offset-checkpointing.md, "Stream-open cost"):
--   agent_tokens index scans   token lookups (ValidateAgentToken); with the auth
--                              cache this should NOT rise with stream opens
--   organizations/namespaces   the two Preloads of that lookup; they move with it
--   agent_tokens row updates   the last_seen_* stamps; batched, so about one
--                              statement per flush interval per coordinator,
--                              independent of how many streams open
--   db transactions            whole-database load, for before/after comparison
--
-- Counters are shared by every caller (API, CLI, web), so they include traffic
-- other than weftd ingest; compare runs against each other, not against zero.

CREATE TEMP TABLE _tables0 AS
SELECT clock_timestamp() AS t, relname, idx_scan, n_tup_upd
FROM pg_stat_user_tables
WHERE relname IN ('agent_tokens', 'organizations', 'namespaces');

CREATE TEMP TABLE _db0 AS
SELECT clock_timestamp() AS t,
       xact_commit + xact_rollback AS xacts,
       tup_updated
FROM pg_stat_database
WHERE datname = current_database();

SELECT pg_sleep(30);

-- Postgres 15+ caches stats reads for the rest of the transaction. A
-- multi-statement run can be one transaction, which would make the second
-- read identical to the first and report zero rates. Drop the cache.
SELECT pg_stat_clear_snapshot();

SELECT metric,
       delta,
       round(delta / secs, 2) AS per_second,
       round(secs)            AS window_s
FROM (
    SELECT s.relname || ' index scans' AS metric,
           n.idx_scan - s.idx_scan     AS delta,
           extract(epoch FROM clock_timestamp() - s.t) AS secs,
           1 AS ord
    FROM _tables0 s
    JOIN pg_stat_user_tables n ON n.relname = s.relname

    UNION ALL
    SELECT 'agent_tokens row updates',
           n.n_tup_upd - s.n_tup_upd,
           extract(epoch FROM clock_timestamp() - s.t),
           2
    FROM _tables0 s
    JOIN pg_stat_user_tables n ON n.relname = s.relname
    WHERE s.relname = 'agent_tokens'

    UNION ALL
    SELECT 'db transactions',
           (n.xact_commit + n.xact_rollback) - d.xacts,
           extract(epoch FROM clock_timestamp() - d.t),
           3
    FROM _db0 d, pg_stat_database n
    WHERE n.datname = current_database()

    UNION ALL
    SELECT 'db rows updated',
           n.tup_updated - d.tup_updated,
           extract(epoch FROM clock_timestamp() - d.t),
           4
    FROM _db0 d, pg_stat_database n
    WHERE n.datname = current_database()
) r
ORDER BY ord, metric;
