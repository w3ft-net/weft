-- +goose Up
-- has_seen_welcome drives the first-time login modal. New rows default
-- false; existing users (imported before the welcome tour existed)
-- default true so we don't retroactively show them the onboarding flow.
ALTER TABLE public.users
    ADD COLUMN has_seen_welcome BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE public.users SET has_seen_welcome = TRUE;

ALTER TABLE public.users
    ALTER COLUMN has_seen_welcome SET DEFAULT FALSE;

-- +goose Down
ALTER TABLE public.users DROP COLUMN has_seen_welcome;
