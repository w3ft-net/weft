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

### `TestOnboardFlow` (integration/onboard_test.go) — ✅ resolved (2026-04-27, a9a75ab)

The original timeout was test-setup, not a product bug. weftd's
Tailer (weftd/tailer.go:30) Seek(0, io.SeekEnd)s on open —
production-correct, since restarts shouldn't replay the whole log.
The 6 fixture lines in dev/testapp.log were behind that cursor by
the time configuration completed, so nothing flowed. Cold-start had
an additional race against weftd's 5-second `WaitForConfiguration`
ticker. Fix snapshots the file, appends a uniquely-tagged line on
each poll iteration, and asserts on the tag with `| grep`. Cleanup
restores the fixture.

### `TestBuckets_OnboardAppPropagates` (integration/buckets_test.go) — ✅ resolved (2026-04-27)

The "1 of 3 shards consistently misses propagation" failure is gone.
Verified at 2-shard config (5/5 green) and at 3-shard config after
manually bumping `buckets.target_ingesting_count` (5/5 green); test
asserts every INGESTING shard receives the new app via the
reconciler's `AddAppToShardOp` fanout. Likely fixed by some
combination of intermediate work; the most relevant change is
`aea6d7e coordinator: drop "empty Apps = all apps" catch-all`,
which made any propagation miss surface as missing query data
rather than being masked by the catch-all.

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

### `TestLambdaColdShard_DockerIntegration` — ✅ resolved (2026-04-28)

The lambda-shard compose service was collateral damage in
5a175bf (Provisioner manages shard lifecycle in dev and prod —
ripped the static shard containers, took the lambda-shard
container with it). The Dockerfile still built the binary and
the test still expected :9030 to answer, so it dialed and got
"connection refused" in 50ms. Restored the lambda-shard service
in docker-compose.yml (S3_* env shape per 05a0a6f's WEFT_-prefix
drop) and tagged lambda_test.go with //go:build integration so it
runs alongside the other heavy tests. 5/5 green; cold-shard query
via the LambdaColdShard path is back in coverage.

### `TestLifecycle_Integration_ChristenPath` — ✅ resolved (2026-04-27, 9927ae4)

Two stacked problems hidden behind one unreachable endpoint.
LocalStack on :4566 was never running, so every AWS-SDK-touching
test timed out at the first client call — added
`docker-compose.localstack.yml` overlay (SNS/SQS/EC2; ASG isn't in
Community, so the one ASG-touching test stubs that client). With
LocalStack up, christen exposed real test bitrot: the path got a
ShardOps requirement when ASG desired_capacity moved to the
reconciler, but the test never threaded one through. Now seeds the
reconciler-minted LAUNCHING row, passes a real ShardOps, and
asserts the volume tag carries the seeded shard id.

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
