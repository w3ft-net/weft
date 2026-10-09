-- Compact counter snapshot for before/after deltas. Run twice ~10 minutes apart
-- (append both outputs to one file, or send them separately) and diff the numbers.

SELECT now() AS taken_at;

SELECT blks_read, blks_hit, xact_commit, xact_rollback, deadlocks, temp_files
FROM pg_stat_database WHERE datname = 'weft';

-- Statements that matter to the plan. Matched by prefix so the old and new
-- query texts show as separate rows; the old ones should stop growing.
SELECT calls,
       round(total_exec_time::numeric)        AS total_ms,
       round(mean_exec_time::numeric, 2)      AS mean_ms,
       shared_blks_read, shared_blks_hit, rows,
       left(regexp_replace(query, '\s+', ' ', 'g'), 110) AS query
FROM pg_stat_statements
WHERE query ILIKE '%shard_heartbeats%'
   OR query ILIKE 'UPDATE%shard_records%'
   OR query ILIKE 'UPDATE%users%'
   OR query IN ('BEGIN', 'COMMIT')
   OR query ILIKE 'CALL public.rollup%'
   OR query ILIKE 'SELECT * FROM "cron_results" WHERE cron_watch_id%'
   AND query NOT ILIKE '%pg_stat_statements%'
ORDER BY query;

SELECT relname, n_tup_ins, n_tup_upd, n_tup_hot_upd, n_tup_del, n_dead_tup, seq_scan, idx_scan
FROM pg_stat_user_tables
WHERE relname IN ('shard_heartbeats', 'shard_records', 'users', 'cron_claims', 'alert_incidents', 'usage_hourly_default')
ORDER BY relname;
