-- Why is shard_heartbeats_default 1 GB? (follow-up to db-baseline-step0.sql)

-- Child partitions of shard_heartbeats and their sizes
SELECT c.relname,
       pg_size_pretty(pg_total_relation_size(c.oid)) AS total,
       pg_get_expr(c.relpartbound, c.oid)            AS bound
FROM pg_inherits i
JOIN pg_class c ON c.oid = i.inhrelid
WHERE i.inhparent = 'public.shard_heartbeats'::regclass
ORDER BY c.relname;

-- Partition key and column names
SELECT pg_get_partkeydef('public.shard_heartbeats'::regclass) AS partkey;
SELECT column_name, data_type FROM information_schema.columns
WHERE table_name = 'shard_heartbeats' ORDER BY ordinal_position;

-- partman config for it
SELECT parent_table, control, partition_interval, premake, retention, retention_keep_table, automatic_maintenance
FROM partman.part_config WHERE parent_table = 'public.shard_heartbeats';

-- Relation stats: pages vs. live tuples, vacuum history
SELECT relname, relpages, reltuples::bigint,
       pg_size_pretty(pg_relation_size(oid)) AS heap
FROM pg_class WHERE relname = 'shard_heartbeats_default';
SELECT last_vacuum, last_autovacuum, last_analyze, last_autoanalyze,
       n_tup_ins, n_tup_del, n_live_tup, n_dead_tup
FROM pg_stat_user_tables WHERE relname = 'shard_heartbeats_default';

-- Who is scanning it
SELECT calls, round(total_exec_time::numeric) AS total_ms,
       round(mean_exec_time::numeric, 2) AS mean_ms,
       shared_blks_read, shared_blks_hit, rows,
       left(query, 200) AS query
FROM pg_stat_statements
WHERE query ILIKE '%shard_heartbeats%'
ORDER BY shared_blks_read DESC
LIMIT 10;

-- Bloat check (only works if pgstattuple is available; expected to error otherwise)
-- CREATE EXTENSION IF NOT EXISTS pgstattuple;
-- SELECT * FROM pgstattuple('shard_heartbeats_default');
