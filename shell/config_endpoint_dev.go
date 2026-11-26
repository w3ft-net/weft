//go:build !release

package shell

import "os"

// EndpointOverride lets internal tooling — chaos tests pinning a client
// to one coordinator instance, staging/dev pointing at a non-default API
// host — retarget the CLI via WEFT_ENDPOINT. This is not a public API:
// customers have no legitimate reason to redirect the CLI away from
// their configured endpoint, so release builds compile it out entirely
// (see config_endpoint_release.go and the `release` build tag applied in
// .kevcons/ext/release.lua).
func EndpointOverride() string {
	return os.Getenv("WEFT_ENDPOINT")
}
