-- +goose Up
--
-- Track email ownership verification for each user. Nil = unverified; a
-- timestamp = when ownership was proven by clicking a magic link sent to
-- that email address. The api middleware gates state-changing routes on
-- this column and a background sweep deletes unverified accounts older
-- than 24h.

ALTER TABLE users ADD COLUMN email_verified_at TIMESTAMPTZ;

-- Grandfather every existing user as verified. Pre-launch the audit
-- found that signup was issuing JWTs without any email check at all, so
-- "verified" here means "we trust the account because it predates the
-- verification requirement" rather than "we literally round-tripped an
-- email."
UPDATE users SET email_verified_at = now() WHERE email_verified_at IS NULL;

-- +goose Down
ALTER TABLE users DROP COLUMN IF EXISTS email_verified_at;
