-- +goose Up
-- Audit log for the daily meter-event reporter. One row per
-- (customer, day) that we successfully posted to Stripe; the row is
-- written only after the API call succeeds, so the absence of a row
-- signals "needs report" on the next run. The UNIQUE constraint is
-- the weft-side idempotency guard; Stripe has its own via the
-- meter event's `identifier` field.
--
-- See docs/plans/stripe.md for the overage-billing plan.

CREATE TABLE public.stripe_usage_reports (
    id               BIGSERIAL PRIMARY KEY,
    customer_name    TEXT        NOT NULL,
    day              DATE        NOT NULL,
    kb_reported      BIGINT      NOT NULL,
    stripe_event_id  TEXT        NOT NULL,
    reported_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (customer_name, day)
);

CREATE INDEX idx_stripe_usage_reports_day
    ON public.stripe_usage_reports (day);

-- +goose Down
DROP TABLE IF EXISTS public.stripe_usage_reports;
