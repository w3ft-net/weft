-- +goose Up
--
-- Rename `accounts` → `aws_accounts` to make the CloudWatch-specific
-- semantics explicit. Weftd sources don't have AWS credentials and with
-- migration 012 their log_groups.account_id is NULL, so the table now
-- exclusively represents AWS-account registrations for cross-account
-- CloudWatch reads.

ALTER TABLE accounts RENAME TO aws_accounts;

-- Disambiguate the column holding the 12-digit AWS account identifier.
-- Before: aws_accounts.aws_account (awkward once the table is aws_accounts).
-- After:  aws_accounts.account_id  (reads as "the AWS account's ID").
ALTER TABLE aws_accounts RENAME COLUMN aws_account TO account_id;

-- Rename the FK column so the relationship is self-evident at the log_groups
-- row level.
ALTER TABLE log_groups RENAME COLUMN account_id TO aws_account_id;

-- +goose Down
ALTER TABLE log_groups RENAME COLUMN aws_account_id TO account_id;
ALTER TABLE aws_accounts RENAME COLUMN account_id TO aws_account;
ALTER TABLE aws_accounts RENAME TO accounts;
