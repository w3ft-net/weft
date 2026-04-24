# Integration test bitrot follow-up

During prep for the direct-to-compute work
(docs/plans/direct-to-compute.md), the existing integration suite
turned out to have multiple pre-existing failures. The harness
issues (JWT secret mismatch, missing fixtures, a missing sample
log file, and two tests that predate single-JWT auth) were cleared
as a prerequisite for running coverage-instrumented tests reliably.

What remains is a shorter list of tests that fail for reasons that
**aren't test infrastructure** — they may be real bugs in the
product, or test logic that has drifted out of sync with behaviour
the product now has. None of them are on the direct-to-compute
critical path, but they deserve triage so the regressions are tracked
rather than silently accepted as baseline noise.

## Failing tests

### `TestOnboardFlow` (integration/onboard_test.go)

**Where it fails:** after `configureSource()` returns 200, the test
polls `cat /log/testapp.log` for the rows weftd should now be
streaming. It times out at 60 seconds seeing zero rows.

**Suspicion:** weftd isn't transitioning from "sampled, waiting"
into "streaming" after the API approves the pending source. Either
weftd doesn't notice the config change, or the pending→configured
transition isn't propagated back down the source-status wire, or
weftd is streaming but the data isn't making it into a shard.

**How to diagnose:**
- Watch `docker compose logs weftd -f` during a fresh test run.
- Check the coordinator's shard-write path for this row batch.
- Check the /api/sources/<ns>/<app>/configure handler's post-
  approval behaviour — does it push anything to weftd?

### `TestBuckets_OnboardAppPropagates` (integration/buckets_test.go)

**Where it fails:** after `AddSource` succeeds and buckets get the
new app name propagated, the test walks every INGESTING shard in
the default bucket and asserts each one has the new app in its
`shard_records.apps` list. One of the three INGESTING shards
consistently misses the propagation.

**Suspicion:** race between bucket-level propagation and per-shard
`shard_records.apps` update. The coordinator broadcasts the new
app to the bucket but the enqueue-to-every-shard step isn't atomic
with the bucket write.

**How to diagnose:** look at the coordinator's `AddAppToShard` /
bucket-onboarding code path and check whether every INGESTING
shard in the bucket is actually iterated, or whether there's an
early-exit that misses one.

### `TestDockerStack_IngestAndQuery`, `TestDockerStack_JqFieldExtraction`

**Where they fail:** pass cleanly when run in isolation
(`go test -run TestDockerStack_IngestAndQuery`); fail when run
under the full suite.

**Suspicion:** shared-state pollution between tests — an earlier
test leaves rows or shard state that poisons these queries' row-
count assertions. The assertion "expected 20 rows, got N" where
N > 20 points at cumulative residue.

**How to diagnose:** add a `t.Cleanup` to each test that deletes
everything it ingested (by customer+app), or switch the assertion
from exact-count to "at least the rows I ingested are present."

### `TestLambdaColdShard_DockerIntegration`

**Where it fails:** haven't dug in. Fails fast (~0.05s), probably
an auth or setup issue similar to what the bitrot pass already
fixed; just hasn't been looked at.

**Next step:** rerun in isolation with `-v`, share the failure
mode, triage from there.

### `TestLifecycle_Integration_ChristenPath`

**Where it fails:** 30-second timeout (hits the default shard-
adoption deadline). Probably waiting for something that used to
happen but no longer does after an unrelated change.

**Next step:** rerun in isolation with `-v`, share the failure
mode, triage from there.

## Why this is a doc rather than a fix

Every one of the above is likely half a day to a day of focused
investigation. They aren't in the direct-to-compute path and
debugging each would extend the prep phase indefinitely. Capturing
them here keeps the baseline signal honest — new direct-to-compute
tests run against a suite where "these six tests are known to fail
and why" — without letting the list grow unchecked.

New direct-to-compute tests are written hermetically (each creates
its own namespace + data, tears down on completion) so they don't
participate in the ordering-flakiness class.
