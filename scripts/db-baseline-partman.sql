-- Why did partman stop creating shard_heartbeats partitions? (follow-up to db-baseline-heartbeats.sql)

-- pg_cron jobs (partman maintenance should be here)
SELECT jobid, schedule, command, database, active FROM cron.job ORDER BY jobid;

-- Recent runs of anything partman-related, newest first, with failures
SELECT d.jobid, d.status, d.start_time, d.end_time, left(d.return_message, 300) AS msg
FROM cron.job_run_details d
JOIN cron.job j USING (jobid)
WHERE j.command ILIKE '%partman%'
ORDER BY d.start_time DESC
LIMIT 20;

-- Last successful run vs. last failure per job
SELECT jobid, status, max(start_time) AS last_start, count(*) AS runs
FROM cron.job_run_details
WHERE jobid IN (SELECT jobid FROM cron.job WHERE command ILIKE '%partman%')
GROUP BY jobid, status
ORDER BY jobid, status;

-- Time range of what is sitting in the default partition (index-assisted; avoid count(*))
SELECT (SELECT min(beat_at) FROM shard_heartbeats_default) AS oldest,
       (SELECT max(beat_at) FROM shard_heartbeats_default) AS newest;
