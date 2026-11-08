# Service commitments for self-serve launch

> **Status (2026-11-08): decided in an interview with Paul; nothing built.**
> The text under "Draft customer-facing wording" is a plain-language draft for
> a lawyer to review, not legal advice, and nothing here is published.

## Decisions

| Question | Decision |
|---|---|
| Posture | **No SLA.** No numeric uptime commitment and no credits, on any self-serve plan. A status page reports how the service is doing. Bolt is negotiated per contract and is out of scope here. |
| What the status page reports | Query and shell, ingest acceptance, and cron and alert execution. Cold history queries are not reported separately. |
| Credits | None. |
| Data | Best effort, no durability promise. Retention is the window we aim to keep data for, not a guarantee against loss. Customers keep their own copy at the source. |

Why this posture: the facts that would have to back a number are not in place.
`weft-postgres` is single-AZ and shared with vimgym, so one host loss takes
every customer's auth, crons and usage down. Multi-AZ is deferred and cells are
mid-rollout (`cells-dynamo-rollout.md`, `cost-and-pricing.md` section 1d).
Ingest is acknowledged before it is archived. Revisit when Multi-AZ and the
second cell are live and `coordinator-chaos-tests.md` has been run: that is the
point at which a number can be backed by something measured.

## What has to exist before launch

1. **A status page.** None exists anywhere in the repo (`data-residency.md`
   mentions one as a global service, but it is not built). Minimum: three
   components matching the table above, a manual incident log, and a link from
   the landing page footer and the account page. It must not depend on
   `weft-postgres` or the coordinators, or it goes down with the thing it
   reports. Hosting choice is open.
2. **Terms of service and a privacy policy.** Neither exists in the frontend or
   the manual. Signup should link to both and record acceptance (a timestamp on
   the user or the org), which it does not do today.
3. **A support contact** named in the terms, with no response-time promise.
4. **Wording on the pricing page and the signup step** that matches the above:
   no "uptime" or "99.x%" language anywhere, and "retention" described as the
   window we keep data for.

## Draft customer-facing wording

For the terms of service and a short "How weft is run" manual page.

**Availability.** weft is provided on an as-available basis. We work to keep it
running and we publish its current state, and past incidents, on our status
page. We do not promise a particular level of uptime, and we do not offer
service credits for downtime. Planned maintenance is announced on the status
page where we can.

**Your data.** Each plan keeps your logs for a retention window (Swatch 30
days, Yard 90 days). We aim to keep data for that long and delete it after.
Logs are accepted into weft before they are archived, and we do not promise
that data will never be lost or unavailable. Keep your own copy of anything you
cannot afford to lose. Deleted data is not recoverable.

**Cold history.** Queries that reach back past the recent window read archived
data, up to a monthly allowance per plan. When the allowance is used up, those
queries return an error that says when it resets; recent data is unaffected.

**Trials and billing.** A trial lasts 14 days and needs a card. Nothing is
charged until the trial ends; cancel before then and you are not charged. After
that the plan is billed monthly, with ingestion beyond your plan's included
volume billed at the plan's per-GB rate.

**Changes.** We may change these terms. We will tell you before a change that
affects what you pay or how your data is handled.

## Open

- Legal entity name and governing law for the terms (not in the repo).
- Whether a data processing agreement is needed for customers who ship personal
  data in logs; `compliance/field-redaction.md` and `ingest-governance.md` are
  the related product features.
- Where the status page is hosted, and who updates it during an incident.
- Whether Yard customers will expect more than this once they pay $99 a month.
  Paul chose no credits on any plan; the alternative was credits for Yard only.
