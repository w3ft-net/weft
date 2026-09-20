//go:build release

package shell

// EndpointOverride is a no-op in release builds: WEFT_ENDPOINT is
// internal-only tooling (see config_endpoint_dev.go) with no legitimate
// customer use, so release builds — tagged `release` by
// .kevcons/ext/release.lua — never honor it.
func EndpointOverride() string {
	return ""
}
