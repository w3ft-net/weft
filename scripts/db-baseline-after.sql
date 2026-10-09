-- Re-baseline after the heartbeat redesign + items 2-4 deploy.
-- Compare against the 2026-10-08 numbers in docs/plans/db-optimization-pass.md.

-- 1. Did the migrations land? (expect 82 applied, table is a plain relation, one trigger pair)
SELECT max(version_id) AS goose_version FROM goose_db_version WHERE is_applied;
SELECT relkind::text AS shard_heartbeats_relkind FROM pg_class WHERE oid = 'public.shard_heartbeats'::regclass;
SELECT count(*) AS shard_heartbeats_partman_rows FROM partman.part_config WHERE parent_table = 'public.shard_heartbeats';
SELECT tgname, (tgqual IS NOT NULL) AS has_when FROM pg_trigger
WHERE tgrelid = 'public.shard_records'::regclass AND NOT tgisinternal ORDER BY tgname;
SELECT count(*) FILTER (WHERE disk_bytes > 0) AS shards_with_disk_bytes, count(*) AS shards FROM shard_records;

-- 2. Database size and cache hit rate (was: 1105 MB, 35.15% hit, 953M blocks read)
SELECT datname, numbackends, xact_commit, blks_read, blks_hit,
       round(100.0 * blks_hit / NULLIF(blks_hit + blks_read, 0), 2) AS hit_pct,
       temp_files, deadlocks, stats_reset
FROM pg_stat_database WHERE datname = 'weft';
SELECT pg_size_pretty(pg_database_size('weft')) AS weft_size;

-- 3. Largest relations (was: shard_heartbeats_default 1003 MB, everything else < 20 MB)
SELECT c.relname,
       pg_size_pretty(pg_total_relation_size(c.oid)) AS total,
       s.n_live_tup, s.n_dead_tup, s.seq_scan, s.seq_tup_read, s.idx_scan,
       s.n_tup_upd, s.n_tup_hot_upd
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
WHERE c.relkind IN ('r','p')
ORDER BY pg_total_relation_size(c.oid) DESC
LIMIT 12;

-- 4. The new heartbeat table's contents (expect ~one row per live shard plus recent replaced instances)
SELECT shard_id, instance_id, state, disk_bytes, now() - beat_at AS age
FROM shard_heartbeats ORDER BY beat_at DESC LIMIT 20;

-- 5. Is partman maintenance healthy again? (was: every run failing since 2026-10-06 21:00)
SELECT status, count(*) AS runs, max(start_time) AS last_start
FROM cron.job_run_details
WHERE jobid = 1 AND start_time > now() - interval '3 hours'
GROUP BY status ORDER BY status;
SELECT status, start_time, left(return_message, 200) AS msg
FROM cron.job_run_details WHERE jobid = 1 ORDER BY start_time DESC LIMIT 5;

-- 6. The queries that mattered, by blocks read. The stats are cumulative since the
-- last reset, so the OLD adopt query (DISTINCT ON ... ORDER BY shard_id, beat_at DESC
-- without the UNION) keeps its historical totals; look at whether its calls still grow
-- and at the NEW text's mean_ms.
SELECT calls, round(mean_exec_time::numeric, 2) AS mean_ms, round(total_exec_time::numeric) AS total_ms,
       shared_blks_read, shared_blks_hit, left(regexp_replace(query, '\s+', ' ', 'g'), 160) AS query
FROM pg_stat_statements
WHERE query ILIKE '%shard_heartbeats%' AND query NOT ILIKE '%pg_stat_statements%'
ORDER BY shared_blks_read DESC LIMIT 8;

-- 7. Top 10 statements by total blocks read, database-wide
SELECT calls, round(mean_exec_time::numeric, 2) AS mean_ms,
       shared_blks_read, shared_blks_hit,
       left(regexp_replace(query, '\s+', ' ', 'g'), 140) AS query
FROM pg_stat_statements
WHERE query NOT ILIKE '%pg_stat_statements%'
ORDER BY shared_blks_read DESC LIMIT 10;

-- 8. Item 2/3: write volume on users, and BEGIN/COMMIT chatter
SELECT calls, rows, left(regexp_replace(query, '\s+', ' ', 'g'), 120) AS query
FROM pg_stat_statements
WHERE query ILIKE 'UPDATE%users%' OR query IN ('BEGIN', 'COMMIT')
ORDER BY calls DESC LIMIT 8;

-- 9. Connections by application and state
SELECT coalesce(nullif(application_name, ''), '(none)') AS app, state, count(*)
FROM pg_stat_activity WHERE datname = 'weft' GROUP BY 1, 2 ORDER BY 3 DESC;
