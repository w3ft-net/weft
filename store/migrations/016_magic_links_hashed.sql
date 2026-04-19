-- +goose Up
--
-- Magic-link tokens are now stored as sha256 hex digests instead of
-- the plaintext bytes emailed to the user (audit finding #2). The
-- emailed URL still carries the raw token; the coordinator hashes on
-- validate+consume. Any rows present before this migration hold
-- plaintext values that would no longer match once the Go code starts
-- hashing before lookup, so they would be effectively dead anyway —
-- drop them so nothing is accidentally usable during the transition
-- window. Magic links expire in 15 minutes so the worst case for a
-- user mid-login is "request the link again."

DELETE FROM magic_links;

-- +goose Down
-- No-op: we can't un-hash the tokens we never stored.
SELECT 1;
