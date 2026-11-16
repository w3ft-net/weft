package shardstore

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
)

// GenerateShardHeartbeatToken mints a fresh 256-bit random bearer token
// for a newly-christened shard. Callers store the result on the shard
// and hand the same string back to the shard via the Configure RPC.
func GenerateShardHeartbeatToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("heartbeat token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ConstantTimeTokenEqual compares two heartbeat tokens in constant
// time. Returns false if either side is empty — an empty stored token
// means "no token to authenticate against" and must never match an
// empty presented bearer.
func ConstantTimeTokenEqual(stored, presented string) bool {
	if stored == "" || presented == "" {
		return false
	}
	if len(stored) != len(presented) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(presented)) == 1
}
