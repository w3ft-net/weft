-- +goose Up
-- Lines a customer's queries scanned from cold storage, by day
-- (docs/plans/cold-scan-cap.md). Append-only with no unique key, like
-- usage_hourly: each coordinator inserts its own delta and readers SUM, so
-- concurrent coordinators never race on a shared row.
CREATE TABLE public.cold_scan_daily (
    org_id       bigint NOT NULL,
    day          date   NOT NULL,
    rows_scanned bigint NOT NULL DEFAULT 0
);
CREATE INDEX idx_cold_scan_daily_org_day ON public.cold_scan_daily (org_id, day);

-- +goose Down
DROP TABLE IF EXISTS public.cold_scan_daily;
