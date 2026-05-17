-- +goose Up

-- Backfill the partman + pg_cron imperative setup that the old
-- collapse mechanism silently dropped (pg_dump serializes partitioned
-- tables and templates but NOT part_config rows, cron.job rows, or the
-- create_parent/cron.schedule calls; the migrations that ran them were
-- then deleted by collapse). Reconstruction derived from prod
-- part_config / cron.job — see docs and the collapse change that keeps
-- this regenerated from here on.
--
-- Idempotent: each create_parent is guarded by an IF NOT EXISTS on
-- part_config (no-op on prod, where it already exists; establishes it
-- on weft_test / CI / any fresh DB). The part_config UPDATEs re-assert
-- the canonical retention/premake. cron.schedule upserts by job name.
--
-- The cron block is wrapped in an EXCEPTION handler: pg_cron only
-- exists in the cron database (prod `weft`); on every other database
-- (weft_test, CI) it was deliberately skipped at bootstrap, so
-- cron.schedule won't resolve there and must be tolerated, exactly
-- like guardPgCronExtension does for the extension itself.

-- +goose StatementBegin
DO $wpm$ BEGIN IF NOT EXISTS (SELECT 1 FROM partman.part_config WHERE parent_table='public.cron_results') THEN PERFORM partman.create_parent(p_parent_table:='public.cron_results',p_control:='ran_at',p_type:='range',p_interval:='1 day',p_template_table:='partman.template_public_cron_results',p_premake:=3,p_automatic_maintenance:='on',p_default_table:=false); END IF; END $wpm$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $wpm$ BEGIN IF NOT EXISTS (SELECT 1 FROM partman.part_config WHERE parent_table='public.cron_results_4h') THEN PERFORM partman.create_parent(p_parent_table:='public.cron_results_4h',p_control:='bucket_start',p_type:='range',p_interval:='7 days',p_template_table:='partman.template_public_cron_results_4h',p_premake:=3,p_automatic_maintenance:='on',p_default_table:=false); END IF; END $wpm$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $wpm$ BEGIN IF NOT EXISTS (SELECT 1 FROM partman.part_config WHERE parent_table='public.cron_results_daily') THEN PERFORM partman.create_parent(p_parent_table:='public.cron_results_daily',p_control:='bucket_start',p_type:='range',p_interval:='1 mon',p_template_table:='partman.template_public_cron_results_daily',p_premake:=3,p_automatic_maintenance:='on',p_default_table:=false); END IF; END $wpm$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $wpm$ BEGIN IF NOT EXISTS (SELECT 1 FROM partman.part_config WHERE parent_table='public.cron_results_hourly') THEN PERFORM partman.create_parent(p_parent_table:='public.cron_results_hourly',p_control:='bucket_start',p_type:='range',p_interval:='1 day',p_template_table:='partman.template_public_cron_results_hourly',p_premake:=3,p_automatic_maintenance:='on',p_default_table:=false); END IF; END $wpm$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $wpm$ BEGIN IF NOT EXISTS (SELECT 1 FROM partman.part_config WHERE parent_table='public.shard_heartbeats') THEN PERFORM partman.create_parent(p_parent_table:='public.shard_heartbeats',p_control:='beat_at',p_type:='range',p_interval:='01:00:00',p_template_table:='partman.template_public_shard_heartbeats',p_premake:=2,p_automatic_maintenance:='on',p_default_table:=false); END IF; END $wpm$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $wpm$ BEGIN IF NOT EXISTS (SELECT 1 FROM partman.part_config WHERE parent_table='public.tmp_file_lines') THEN PERFORM partman.create_parent(p_parent_table:='public.tmp_file_lines',p_control:='updated_at',p_type:='range',p_interval:='01:00:00',p_template_table:='partman.template_public_tmp_file_lines',p_premake:=4,p_automatic_maintenance:='on',p_default_table:=false); END IF; END $wpm$;
-- +goose StatementEnd
-- +goose StatementBegin
DO $wpm$ BEGIN IF NOT EXISTS (SELECT 1 FROM partman.part_config WHERE parent_table='public.tmp_files') THEN PERFORM partman.create_parent(p_parent_table:='public.tmp_files',p_control:='updated_at',p_type:='range',p_interval:='01:00:00',p_template_table:='partman.template_public_tmp_files',p_premake:=4,p_automatic_maintenance:='on',p_default_table:=false); END IF; END $wpm$;
-- +goose StatementEnd

UPDATE partman.part_config SET retention='7 days',retention_keep_table=false,retention_keep_index=true,infinite_time_partitions=false,premake=3,automatic_maintenance='on' WHERE parent_table='public.cron_results';
UPDATE partman.part_config SET retention='35 days',retention_keep_table=false,retention_keep_index=true,infinite_time_partitions=false,premake=3,automatic_maintenance='on' WHERE parent_table='public.cron_results_4h';
UPDATE partman.part_config SET retention='400 days',retention_keep_table=false,retention_keep_index=true,infinite_time_partitions=false,premake=3,automatic_maintenance='on' WHERE parent_table='public.cron_results_daily';
UPDATE partman.part_config SET retention='14 days',retention_keep_table=false,retention_keep_index=true,infinite_time_partitions=false,premake=3,automatic_maintenance='on' WHERE parent_table='public.cron_results_hourly';
UPDATE partman.part_config SET retention='5 minutes',retention_keep_table=false,retention_keep_index=true,infinite_time_partitions=false,premake=2,automatic_maintenance='on' WHERE parent_table='public.shard_heartbeats';
UPDATE partman.part_config SET retention='48 hours',retention_keep_table=false,retention_keep_index=true,infinite_time_partitions=false,premake=4,automatic_maintenance='on' WHERE parent_table='public.tmp_file_lines';
UPDATE partman.part_config SET retention='48 hours',retention_keep_table=false,retention_keep_index=true,infinite_time_partitions=false,premake=4,automatic_maintenance='on' WHERE parent_table='public.tmp_files';

-- +goose StatementBegin
DO $wcron$ BEGIN
  PERFORM cron.schedule('partman-maintenance','*/30 * * * *','CALL partman.run_maintenance_proc()');
  PERFORM cron.schedule('cron-rollup-hourly','*/5 * * * *','CALL public.rollup_cron_hourly()');
  PERFORM cron.schedule('cron-rollup-4h','15 * * * *','CALL public.rollup_cron_4h()');
  PERFORM cron.schedule('cron-rollup-daily','30 * * * *','CALL public.rollup_cron_daily()');
  PERFORM cron.schedule('usage-rollup-daily','0 * * * *','CALL public.rollup_usage_daily()');
EXCEPTION WHEN OTHERS THEN
  RAISE NOTICE 'weft backfill: cron.schedule unavailable in this database (%), skipping cron jobs', SQLERRM;
END $wcron$;
-- +goose StatementEnd

-- +goose Down
-- Intentionally a no-op. This migration reconstructs pre-existing
-- production partman/cron configuration; a Down that tore it down
-- would destroy live partitioning + retention + scheduled jobs on
-- prod. There is no safe automatic reversal.
SELECT 1;
