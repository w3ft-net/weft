# Plan: BYOK encryption (Parquet archive)

Customer-supplied KMS keys for the cold-storage Parquet archive in S3. Hot-shard (EBS) encryption with customer keys is explicitly out of scope until the architecture changes — current shard model is multi-tenant, which is fundamentally incompatible with per-customer EBS keys. Archive is per-shard but bucket layout can be reorganized to per-customer prefixes; that's the path here.

## Why add this

Enterprise procurement teams ask "can we control the keys used to encrypt our data at rest?" The answer today is "everything is encrypted with weft-managed AWS-owned KMS keys" — true, AWS-defaults-secure, but it doesn't satisfy the customer who wants the legal ability to render their archive unreadable by rotating their own key. That's the BYOK ask.

Two tiers of BYOK:

1. **Customer-managed key in *our* account** (CMK in 220664822695). Cheap and fast to deliver. Customer doesn't control the key access policy; we do. Halfway answer; some buyers accept it.
2. **Customer-managed key in *customer's* account.** Cross-account KMS, weft assumes a role to use it. Same model as today's CloudWatch ingestion. This is the "real" BYOK and the only one large enterprises will accept.

We're going for #2. The infra parallels CW cross-account access, so this isn't a new pattern for the codebase.

## What's in scope vs. out

**In scope:**
- Parquet archive objects in S3 encrypted with customer-supplied KMS key via SSE-KMS.
- Cold-shard reader (`ColdShard` / `LambdaColdShard`) decrypts using the same key.
- Rotation: customer rotates their key; new objects use the new version, old objects stay readable until customer denies the old version.
- Customer-driven "kill switch": revoking weft's permission on the KMS key renders the customer's archive instantly unreadable. Documented as the audit-friendly behavior.

**Out of scope:**
- **EBS volumes (hot shards).** Current shards are multi-tenant — one EBS volume serves many customers' rows under the `customer = '...'` row-level filter. Per-customer EBS encryption keys would force single-tenant shards, which is a 100× operational-cost change and breaks bucketing. Not viable until/unless we offer a single-tenant tier as a separate product.
- **In-transit encryption with customer keys.** TLS terminates at our ALB with our certs. "BYOK in transit" usually means client-side encryption before the data leaves the customer's network, which is a different product (and effectively forfeits server-side query). Defer.
- **Backup encryption.** Postgres backups, EBS snapshots, etc. stay on AWS-managed keys. Discuss only if a customer names it.

## Architecture

### Customer setup

1. Customer creates a KMS key in their AWS account (e.g., `arn:aws:kms:us-east-2:123456789012:key/abc-def-...`).
2. Customer creates an IAM role with a trust policy allowing `arn:aws:iam::220664822695:root` (weft's account) to assume it, conditional on `sts:ExternalId`.
3. The role's permissions policy grants `kms:Encrypt`, `kms:Decrypt`, `kms:GenerateDataKey*`, `kms:DescribeKey` on that key only.
4. Customer enters role ARN + external ID + KMS key ARN in weft's Account → Encryption page.
5. weft probes the configuration: assumes the role, calls `kms:DescribeKey`, surfaces success or the specific failure (trust policy missing, ExternalId mismatch, KMS policy too narrow).

### Write path

The archive process today exports DuckDB to Parquet, uploads to `s3://weft-cold-*` with default SSE. New write path:

1. When archiving a shard, look up the customer for each row's data — but archives are per-shard, not per-customer. Today shards are multi-tenant.
2. **Therefore: archive layout changes to per-customer.** During the existing archive step, partition the export by `customer`, write one Parquet object per customer per shard window: `s3://weft-cold-<region>/<customer-id>/<shard-window>/data.parquet`.
3. Each `PutObject` uses `SSE-KMS` with `SSEKMSKeyId = <customer's key ARN>`, executed under credentials from the customer's role assumption.
4. Customers without BYOK config: existing behavior — SSE with weft-managed key, single object per shard, no per-customer partitioning. Two write paths, gated by the customer record.

### Read path

`ColdShard` and `LambdaColdShard` resolve archive locations per customer-and-shard. With BYOK enabled:

1. Resolve customer's role + key.
2. Assume role, get short-lived credentials.
3. `GetObject` against the per-customer prefix; S3 invokes KMS via the assumed role; decrypted bytes return.
4. Cache the assumed-role credentials for 50 minutes (STS gives 1 hour) to avoid per-query assume.

### Failure modes

- **Customer revokes role trust.** Next archive write fails; coordinator alerts the customer ("we can't write your archive — check your KMS role"). Hot shards still work; cold queries against existing archive still work until the assumed-role credentials expire (≤1 hour). After that, cold queries return an error.
- **Customer deletes the KMS key.** Archive is permanently unreadable. We surface this in the dashboard and refuse to silently appear successful.
- **Customer rotates the key.** Transparent; KMS handles versioning. Old objects use the old version, new objects use the new.

## Components to build

1. **`store.CustomerEncryption`** — new model: `{customer_id, kms_key_arn, role_arn, external_id, status, last_probed_at}`. Status reflects probe results.
2. **`coordinator/encryption_probe.go`** — runs after a customer saves their config; calls `sts:AssumeRole` + `kms:DescribeKey`; sets `status = ok | trust_failed | key_inaccessible | unknown_error`. Re-probes hourly so config drift is visible.
3. **Archive write path** — `coordinator/archiver.go` (or wherever the export lives) gains a per-customer partitioning + per-customer S3 client cache keyed on assumed-role credentials.
4. **Cold reader** — `coordinator/cold_shard.go` looks up per-customer config, uses the right credentials. Existing rows-from-S3 plumbing is unchanged once `s3.GetObject` returns bytes.
5. **Web UI** — Account → Encryption page. Form for role ARN + external ID + KMS key ARN. "Test connection" probe. Status badge. Documentation links.
6. **Audit-log integration** — every config change emits an audit event (`encryption.config_save`, `encryption.config_remove`, `encryption.probe_failed`); folds into `docs/plans/audit-log.md`.
7. **Tier gate** — BYOK is an enterprise-tier feature. Cheap users keep AWS-managed encryption.
8. **Docs** — `docs/manual/byok.md` with the IAM trust policy template, KMS key policy template, and a troubleshooting section.

## Per-customer partitioning consequence

Today the archive is one Parquet object per shard; rows from many customers share the file. Splitting per customer:

- Increases object count by ~Nx where N is the avg customers per shard (small at current scale; meaningful at maturity).
- Lets us bill cold storage per customer directly.
- Enables BYOK without ambiguity.

This partitioning is worth doing even without BYOK; BYOK is what forces the schedule.

## Estimated effort

- Customer encryption model + probe loop: **1 day**
- Archive write path partitioning + per-customer S3 client cache: **2 days**
- Cold reader updates + assume-role credential caching: **1 day**
- Web UI + IAM template generation + test-connection probe: **1.5 days**
- Audit log integration: **0.5 day**
- Docs + IAM template + troubleshooting: **1 day**
- Tests (with localstack KMS or a real test customer key): **1 day**

**Total: ~8 days**.

## Non-goals

- **No EBS-volume BYOK.** See "What's in scope vs. out." Single-tenant shards are a separate product decision.
- **No per-namespace or per-app keys.** One key per customer. Sub-customer key partitioning is unbounded complexity for ambiguous benefit.
- **No client-side encryption.** Logs arrive in plaintext over TLS; the encryption boundary is at-rest only.
- **No automatic key rotation orchestration.** Customer rotates on their own schedule via KMS console / IAM policy. We don't proactively rotate or schedule rotations.
- **No reverse-BYOK ("you key, customer reads").** Symmetric: customer key, customer-controlled access.
