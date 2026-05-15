# Security & Compliance

Plans that exist because a buyer's security team, an auditor, or an
incident will ask about them — plus one defect we found ourselves.
Grouped here because they share a threat model (who can see / change /
exfiltrate tenant data) and tend to be referenced together in
procurement and SOC 2 conversations.

Roadmap phasing and effort estimates live in the parent
[`../README.md`](../README.md); this file is the umbrella that explains
how the pieces relate and which is load-bearing for the others.

## The plans

| Plan | Kind | One-liner |
|---|---|---|
| [audit-log](audit-log.md) | feature | Customer-visible trail of config changes (`/log/_audit.log`). Substrate the others emit into. |
| [ingest-governance](ingest-governance.md) | feature | Pre-shard ordered `drop` / `sample` / `redact` rules. Value never lands on disk. PII + cost. |
| [field-redaction](field-redaction.md) | feature | Read-time per-role JSON-field masking. Value persists; visibility is gated by role. |
| [cross-tenant-coordinator-log](cross-tenant-coordinator-log.md) | **defect** | The shared coordinator log leaks other tenants' query/cron content to the ops namespace. Found in prod; remediation, not a feature. |

## How they relate

- **audit-log is foundational.** ingest-governance, field-redaction, and
  byok-encryption all want to emit config-change events; wiring the
  audit substrate once is cheaper than five plans each inventing a
  trail. Build it before the others if sequencing.
- **The two redaction plans are complementary, not redundant.**
  ingest-governance redaction = "never persist this" (uniform,
  irreversible). field-redaction = "persist it, gate who can read it"
  (role-based, reversible). Serious customers usually want both;
  field-redaction's own doc has the side-by-side.
- **cross-tenant-coordinator-log is the odd one out.** It is not
  roadmap polish — it is a found confidentiality defect with a severity
  that depends on an unanswered access-control question. It should not
  be phased behind the feature work; triage it on its own urgency. It
  also bounds claims the other plans make: "redaction protects tenant
  data" is weaker if a shared ops log already echoes that data.

## Posture note

Treat the feature plans (audit-log, governance, redaction) as
commercial enablers — schedule them against sales pressure. Treat the
defect (cross-tenant-coordinator-log) as a security finding — schedule
it against risk, and resolve the "who can read `/log/weft/*`" question
before deciding how loud it is.
