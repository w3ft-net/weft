-- +goose Up
-- /tmp pseudo-filesystem: per-customer scratch space for saving filtered
-- pipeline results. Two tables, both partitioned by updated_at so
-- pg_partman can drop content past the 48h TTL without a row-by-row
-- delete, and so editing a file (touch / overwrite / append) slides
-- it into a fresh partition simply by bumping updated_at — no
-- delete-and-reinsert dance needed. No FK between them — partman drops
-- partitions with DROP TABLE, which doesn't play with FK cascades
-- across partitioned children; the store layer enforces the
-- relationship on read.

CREATE SEQUENCE IF NOT EXISTS public.tmp_files_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

CREATE TABLE public.tmp_files (
    id           BIGINT      NOT NULL DEFAULT nextval('public.tmp_files_id_seq'),
    customer_id  BIGINT      NOT NULL,
    path         TEXT        NOT NULL,                   -- "/tmp/foo" or "/tmp/sub/bar"
    is_dir       BOOLEAN     NOT NULL DEFAULT FALSE,
    byte_size    BIGINT      NOT NULL DEFAULT 0,         -- cached SUM(length(content))
    line_count   BIGINT      NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),     -- audit only; not the partition key
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id, updated_at)
) PARTITION BY RANGE (updated_at);

ALTER SEQUENCE public.tmp_files_id_seq OWNED BY public.tmp_files.id;

-- Postgres requires partition-key columns in any unique index on a
-- partitioned table, so we can't enforce (customer_id, path)
-- uniqueness at the database level. The store layer is the source of
-- truth: GetTmpFile resolves to the most recently updated row via
-- SELECT … ORDER BY updated_at DESC LIMIT 1, and writes are
-- idempotent against that.
CREATE INDEX idx_tmp_files_customer_path
    ON public.tmp_files (customer_id, path);

CREATE TABLE public.tmp_file_lines (
    file_id     BIGINT      NOT NULL,
    line_num    BIGINT      NOT NULL,
    content     TEXT        NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (file_id, line_num, updated_at)
) PARTITION BY RANGE (updated_at);

CREATE INDEX idx_tmp_file_lines_file_line
    ON public.tmp_file_lines (file_id, line_num);

-- pg_partman: hourly partitions, 2-day retention (48h TTL). Hourly
-- rather than daily so a file written right before midnight still
-- gets close to a full 48 hours before its partition is dropped.
-- Touch/append bumps updated_at, which automatically routes the row
-- into a fresh partition (PG11+ moves rows on partition-key UPDATE).
SELECT partman.create_parent(
    p_parent_table := 'public.tmp_files',
    p_control      := 'updated_at',
    p_interval     := '1 hour',
    p_premake      := 4
);
UPDATE partman.part_config
SET retention = '48 hours', retention_keep_table = false
WHERE parent_table = 'public.tmp_files';

SELECT partman.create_parent(
    p_parent_table := 'public.tmp_file_lines',
    p_control      := 'updated_at',
    p_interval     := '1 hour',
    p_premake      := 4
);
UPDATE partman.part_config
SET retention = '48 hours', retention_keep_table = false
WHERE parent_table = 'public.tmp_file_lines';

-- pg_cron already runs partman-maintenance every 30 min (migration 002).
-- No additional schedule needed here.
