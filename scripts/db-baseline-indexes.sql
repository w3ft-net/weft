-- Step 0 remainder for docs/plans/db-optimization-pass.md: unused indexes, activity, partman, cron, cache ratios.
SELECT now() AS taken_at, stats_reset FROM pg_stat_database WHERE datname = 'weft';

-- Unused / rarely used indexes (excluding PKs and unique constraints), largest first
SELECT s.relname AS tbl, s.indexrelname AS idx, s.idx_scan,
       pg_size_pretty(pg_relation_size(s.indexrelid)) AS size,
       i.indisunique AS uniq, i.indisprimary AS pk
FROM pg_stat_user_indexes s JOIN pg_index i ON i.indexrelid = s.indexrelid
WHERE s.idx_scan < 50 AND NOT i.indisprimary
ORDER BY pg_relation_size(s.indexrelid) DESC
LIMIT 60;

-- Heap/index cache hit ratios per table
SELECT relname, heap_blks_read, heap_blks_hit, idx_blks_read, idx_blks_hit
FROM pg_statio_user_tables
ORDER BY heap_blks_read + idx_blks_read DESC LIMIT 10;

-- Connections by app/state
SELECT coalesce(application_name,'') AS app, state, count(*) FROM pg_stat_activity
WHERE datname = 'weft' GROUP BY 1,2 ORDER BY 3 DESC;

-- partman + cron
SELECT parent_table, retention, premake FROM partman.part_config ORDER BY 1;
SELECT jobid, schedule, left(command, 60) AS command, active FROM cron.job ORDER BY jobid;

-- Retention candidates: sizes and row counts
SELECT relname, n_live_tup, n_dead_tup, pg_size_pretty(pg_total_relation_size(relid)) AS total
FROM pg_stat_user_tables
WHERE relname IN ('ingestion_errors','magic_links','device_codes','alert_incidents','shard_operations','shard_services','shard_records','usage_hourly_default','usage_daily_default')
ORDER BY relname;

-- Item 7 candidates by name, whether or not idx_scan < 50
SELECT s.relname AS tbl, s.indexrelname AS idx, s.idx_scan, pg_size_pretty(pg_relation_size(s.indexrelid)) AS size
FROM pg_stat_user_indexes s
WHERE s.indexrelname ~ '(watch_tag_bucket|tmp_file_lines_file_line|cron_results_deleted_at|shard_operations_ts|memberships_org|agent_tokens_version_seen|deleted_at)'
ORDER BY s.relname, s.indexrelname;
