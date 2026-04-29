-- +goose Up
-- comp_reason marks a subscription as comped (no Stripe billing). Empty
-- string means "paid"; non-empty is the audit-trail string for the grant
-- ("owner", "design-partner-foo", "alpha-bar"). Stripe webhook handlers
-- refuse to mutate rows where comp_reason != '' so a misfire can never
-- accidentally cancel a comp account.

ALTER TABLE public.subscriptions
    ADD COLUMN comp_reason TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE public.subscriptions
    DROP COLUMN comp_reason;
