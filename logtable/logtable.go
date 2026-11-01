// Package logtable names the DuckDB table a shard keeps one log source in.
//
// A shard stores the rows of each (customer, namespace, service, kind) in a
// table of its own, so a query for one log file reads only that file's rows.
// The planner and the shard derive the name from the same key with the same
// function, so the name is a pure function of the key and never changes. See
// docs/plans/per-service-tables.md.
package logtable

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// nameVersion is hashed in so a future key shape can't collide with this one.
const nameVersion = 1

// Name returns the table for a source. Customer-supplied identifiers can hold
// any character (some service names contain slashes), so the name is a hash of
// the key and not the key itself: it is always `l_` and 32 lowercase hex
// digits, safe in SQL without quoting. Each field is length-prefixed, so a
// boundary can't move between namespace, service and kind.
//
// env is not part of the key. A path without an env reads every env, which the
// planner can't enumerate, so env stays a column of the table.
func Name(customerID int64, namespace, service, kind string) string {
	h := sha256.New()
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], nameVersion)
	h.Write(n[:])
	binary.BigEndian.PutUint64(n[:], uint64(customerID))
	h.Write(n[:])
	for _, s := range []string{namespace, service, kind} {
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	return "l_" + hex.EncodeToString(h.Sum(nil)[:16])
}
