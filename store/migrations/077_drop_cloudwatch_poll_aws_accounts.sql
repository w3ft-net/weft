-- +goose Up
-- Poll-mode CloudWatch ingestion (coordinator/poller, cross-account role
-- assumption) is retired in favor of push-based ingestion via Kinesis
-- Data Firehose (docs/plans/cloudwatch-kinesis-subscription.md) —
-- confirmed no enabled source_type=cloudwatch rows exist before this
-- migration lands. aws_accounts existed solely to hold the cross-account
-- role weftd/kinesis sources never needed (log_groups.aws_account_id was
-- always NULL for them). log_group_source_type's 'cloudwatch' enum value
-- is left in place — Postgres can't drop an enum value without
-- recreating the type, not worth it for a value nothing writes anymore.
ALTER TABLE public.log_groups DROP CONSTRAINT IF EXISTS fk_accounts_log_groups;
ALTER TABLE public.log_groups DROP COLUMN IF EXISTS aws_account_id;
DROP TABLE IF EXISTS public.aws_accounts;
DROP SEQUENCE IF EXISTS public.accounts_id_seq;

-- last_poll_at/last_token were the poller's FilterLogEvents checkpoint
-- (poll timestamp + pagination token); nothing writes them once the
-- poller is gone. poll_interval was the ticker cadence for the same
-- poller; weftd/kinesis/direct sources never had a meaningful value
-- for it (always "0s" or unset).
ALTER TABLE public.log_groups DROP COLUMN IF EXISTS last_poll_at;
ALTER TABLE public.log_groups DROP COLUMN IF EXISTS last_token;
ALTER TABLE public.log_groups DROP COLUMN IF EXISTS poll_interval;

-- +goose Down
CREATE SEQUENCE public.accounts_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

CREATE TABLE public.aws_accounts (
    id bigint NOT NULL DEFAULT nextval('public.accounts_id_seq'::regclass),
    created_at timestamp with time zone,
    updated_at timestamp with time zone,
    deleted_at timestamp with time zone,
    account_id text NOT NULL,
    role_arn text NOT NULL,
    external_id text,
    org_id uuid NOT NULL
);
ALTER SEQUENCE public.accounts_id_seq OWNED BY public.aws_accounts.id;
ALTER TABLE ONLY public.aws_accounts ADD CONSTRAINT accounts_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.aws_accounts
    ADD CONSTRAINT fk_organizations_accounts FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;

ALTER TABLE public.log_groups ADD COLUMN aws_account_id bigint;
ALTER TABLE ONLY public.log_groups
    ADD CONSTRAINT fk_accounts_log_groups FOREIGN KEY (aws_account_id) REFERENCES public.aws_accounts(id) ON DELETE CASCADE;

ALTER TABLE public.log_groups ADD COLUMN last_poll_at text;
ALTER TABLE public.log_groups ADD COLUMN last_token text;
ALTER TABLE public.log_groups ADD COLUMN poll_interval text NOT NULL DEFAULT '30s';
