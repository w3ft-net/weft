package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

// ClientVersionMiddleware enforces that callers declare a recent
// X-Weft-Client header. The minimum is read from the MIN_WEFT_CLI_VERSION
// environment variable on every request, so an operator can adjust the
// gate without restarting the API server.
//
// Behavior:
//
//   - MIN_WEFT_CLI_VERSION unset/empty → pass-through (gate disabled).
//   - Header missing → pass-through (lax fallback for partial rollout
//     and for tooling that calls the API by hand with curl).
//   - Header present and version < min → HTTP 426 with a structured JSON
//     body the CLI's HTTP path special-cases into a friendly upgrade
//     prompt.
//
// Skip-on-missing-header is conservative on purpose: tighten to
// require-header by flipping a config later, once every shipping client
// is sending it. See [[feedback_no_legacy_support]] (the post-launch
// version) for why we don't hard-cut on day one.
func ClientVersionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		min := strings.TrimSpace(os.Getenv("MIN_WEFT_CLI_VERSION"))
		if min == "" {
			next.ServeHTTP(w, r)
			return
		}
		raw := r.Header.Get("X-Weft-Client")
		if raw == "" {
			next.ServeHTTP(w, r)
			return
		}
		// Parse "weft/<version>[+<sha>]". Only enforce the gate for
		// `weft` (the CLI); browser shell (`weft-web/<sha>`) ships in
		// lockstep with the API and version-string comparison there
		// would be SHAs, not semver.
		binary, version := splitClientID(raw)
		if binary != "weft" {
			next.ServeHTTP(w, r)
			return
		}
		if clientVersionBelow(version, min) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUpgradeRequired) // 426
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":    "client too old",
				"running":  version,
				"required": min,
				"upgrade":  "run `weft update` to fetch the latest binary",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// splitClientID parses "weft/0.3.0+abc123" into ("weft", "0.3.0").
// Returns ("","") for malformed input — caller treats that the same
// as a missing header (pass-through).
func splitClientID(s string) (binary, version string) {
	slash := strings.Index(s, "/")
	if slash <= 0 {
		return "", ""
	}
	binary = s[:slash]
	rest := s[slash+1:]
	if plus := strings.Index(rest, "+"); plus >= 0 {
		rest = rest[:plus]
	}
	return binary, rest
}

// clientVersionBelow compares two bare version strings ("0.3.0",
// "0.14.99") part-wise, numeric where both sides are integers and
// lexicographic otherwise. Mirrors the coordinator-side helper of the
// same shape; not factored into a shared package because the inputs
// here are pre-parsed (no "<binary>/" prefix) and the comparison is
// trivial.
func clientVersionBelow(v, min string) bool {
	pv := strings.Split(v, ".")
	pm := strings.Split(min, ".")
	n := len(pv)
	if len(pm) > n {
		n = len(pm)
	}
	for i := 0; i < n; i++ {
		var a, b string
		if i < len(pv) {
			a = pv[i]
		}
		if i < len(pm) {
			b = pm[i]
		}
		if c := comparePart(a, b); c != 0 {
			return c < 0
		}
	}
	return false
}

func comparePart(a, b string) int {
	an, aok := parseUintFast(a)
	bn, bok := parseUintFast(b)
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

func parseUintFast(s string) (uint64, bool) {
	if s == "" {
		return 0, true
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
