// Package logtable names the DuckDB table a shard keeps one log source in.
//
// A shard stores the rows of each (partition, namespace, service, kind) in a
// table of its own, so a query for one log file reads only that file's rows.
// The planner and the shard derive the name from the same key with the same
// function, so the name is a pure function of the key and never changes. The
// partition is the batch's values typed as the layout declares them: an int64
// is hashed as eight bytes, a string length-prefixed, so a layout of one int64
// column names the tables the hosted shape always has.
package logtable

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// nameVersion is hashed in so a future key shape can't collide with this one.
const nameVersion = 1

// maxSlug bounds the readable part, so a long key can't make a long name.
const maxSlug = 48

var (
	notAlnum  = regexp.MustCompile(`[^a-z0-9]+`)
	nameShape = regexp.MustCompile(`^l_[a-z0-9_]+_[0-9a-f]{32}$`)
)

// Name returns the table for a source: `l_`, a readable form of the key, `_`, and
// 32 lowercase hex digits of a hash of the key. The name is safe in SQL without
// quoting.
//
// The readable part is only for people looking at a shard: it is the key
// lowercased with anything outside [a-z0-9] turned into `_`, cut to maxSlug. It
// does not identify the key. Customer-supplied identifiers can hold any
// character (some service names contain slashes), and DuckDB compares
// identifiers without regard to case, so `Api` and `api` read the same there.
// The hash is over the exact key and is what keeps two keys apart; each field is
// length-prefixed, so a boundary can't move between namespace, service and kind.
//
// env is not part of the key. A path without an env reads every env, which the
// planner can't enumerate, so env stays a column of the table.
func Name(partition []any, namespace, service, kind string) string {
	h := sha256.New()
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], nameVersion)
	h.Write(n[:])
	for _, v := range partition {
		switch v := v.(type) {
		case int64:
			binary.BigEndian.PutUint64(n[:], uint64(v))
			h.Write(n[:])
		case string:
			binary.BigEndian.PutUint64(n[:], uint64(len(v)))
			h.Write(n[:])
			h.Write([]byte(v))
		default:
			panic(fmt.Sprintf("logtable: partition value of type %T", v))
		}
	}
	for _, s := range []string{namespace, service, kind} {
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	return "l_" + slug(partition, namespace, service, kind) + "_" + hex.EncodeToString(h.Sum(nil)[:16])
}

func slug(partition []any, namespace, service, kind string) string {
	var parts []string
	for _, v := range partition {
		parts = append(parts, fmt.Sprint(v))
	}
	key := strings.Join(append(parts, namespace, service, kind), "_")
	s := strings.Trim(notAlnum.ReplaceAllString(strings.ToLower(key), "_"), "_")
	if len(s) > maxSlug {
		s = strings.TrimRight(s[:maxSlug], "_")
	}
	return s
}

// IsName reports whether name is a table Name produced, so a shard can tell its
// log tables from the rest of its database.
func IsName(name string) bool { return nameShape.MatchString(name) }
