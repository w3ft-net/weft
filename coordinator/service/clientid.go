package service

import (
	"context"
	"strings"

	"google.golang.org/grpc/metadata"
)

// readClientID extracts the "weft-client" gRPC metadata value from an
// incoming context. Empty when the caller didn't set the header (pre-
// version-gate clients, or unit-test stubs).
func readClientID(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get("weft-client")
	if len(vals) == 0 {
		return ""
	}
	return strings.TrimSpace(vals[0])
}

// clientBelow returns true when clientID's version is strictly below
// floor's version. clientID is the agent-supplied "weftd/<version>[+<sha>]"
// string; floor is the operator-supplied bare version like "0.14.0".
//
// Comparison strategy:
//
//   - Strip the "<binary>/" prefix from clientID; strip "+<sha>" from
//     whatever remains.
//   - Split both sides on "." into dotted parts.
//   - Compare parts numerically when both sides parse as integers,
//     lexicographically otherwise (so "0.14.0-rc1" still compares
//     coherently against "0.14.0").
//
// An empty clientID is treated as "below everything" — agents that
// don't declare a version are old enough to deserve the gate.
func clientBelow(clientID, floor string) bool {
	if floor == "" {
		return false
	}
	if clientID == "" {
		return true
	}
	v := stripClientIDVersion(clientID)
	return compareVersions(v, floor) < 0
}

// stripClientIDVersion turns "weftd/0.14.0+abc123" into "0.14.0".
func stripClientIDVersion(clientID string) string {
	if i := strings.Index(clientID, "/"); i >= 0 {
		clientID = clientID[i+1:]
	}
	if i := strings.Index(clientID, "+"); i >= 0 {
		clientID = clientID[:i]
	}
	return clientID
}

// compareVersions returns -1, 0, or 1 comparing dotted version strings
// part-wise. Numeric where possible, lexicographic otherwise. Shorter
// version is treated as zero-padded ("0.14" == "0.14.0").
func compareVersions(a, b string) int {
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var as, bs string
		if i < len(pa) {
			as = pa[i]
		}
		if i < len(pb) {
			bs = pb[i]
		}
		if c := compareVersionPart(as, bs); c != 0 {
			return c
		}
	}
	return 0
}

func compareVersionPart(a, b string) int {
	an, aok := parseUint(a)
	bn, bok := parseUint(b)
	if aok && bok {
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		default:
			return 0
		}
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func parseUint(s string) (uint64, bool) {
	if s == "" {
		return 0, true // missing part = 0
	}
	var n uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	return n, true
}
