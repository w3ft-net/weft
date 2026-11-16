package shard

import (
	"fmt"
)

// partitionExpr is the _part expression for minutes-wide buckets of the
// timestamp expression ts. The epoch origin makes a bucket's start computable
// from the width alone (BucketStart).
func partitionExpr(minutes int, ts string) string {
	return fmt.Sprintf("time_bucket(INTERVAL '%d minutes', %s, TIMESTAMP '1970-01-01')", minutes, ts)
}
