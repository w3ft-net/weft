package shard

import "time"

// SetPauseHoldTimeoutForTest shortens the pause hold for a test and returns
// the restore func. Test-only: visible to package shard_test, not to
// production code.
func SetPauseHoldTimeoutForTest(d time.Duration) (restore func()) {
	prev := pauseHoldTimeout
	pauseHoldTimeout = d
	return func() { pauseHoldTimeout = prev }
}
