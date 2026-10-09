-- Step 0 baseline for docs/plans/db-optimization-pass.md (partial: sizes + item 1)

-- Top 15 relations by total size (heap + indexes + toast), with dead tuples and scan counts
SELECT
  c.relname,
  pg_size_pretty(pg_total_relation_size(c.oid)) AS total,
  pg_size_pretty(pg_relation_size(c.oid))       AS heap,
  pg_size_pretty(pg_indexes_size(c.oid))        AS idx,
  s.n_live_tup,
  s.n_dead_tup,
  s.seq_scan,
  s.seq_tup_read,
  s.idx_scan,
  s.n_tup_upd,
  s.n_tup_hot_upd
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
WHERE c.relkind IN ('r','p')
ORDER BY pg_total_relation_size(c.oid) DESC
LIMIT 15;

-- Item 1 specifics
SELECT 'usage_hourly_default' AS rel,
       pg_size_pretty(pg_total_relation_size('usage_hourly_default')) AS size,
       (SELECT count(*) FROM usage_hourly) AS rows,
       (SELECT min(hour) FROM usage_hourly) AS oldest;

-- Confirms nothing manages it (usage_hourly should be absent)
SELECT parent_table, retention, premake FROM partman.part_config ORDER BY 1;
