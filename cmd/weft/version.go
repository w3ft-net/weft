package main

// Version is the released semver. Source of truth in source; the
// release ext refuses to publish if its ldflags value disagrees with
// the file. Built as var (not const) so -ldflags "-X main.Version=…"
// can override at link time.
var Version = "0.12.0"

// GitSHA is the short HEAD SHA the binary was built from. Set by
// the release ext via -ldflags "-X main.GitSHA=…"; empty in
// developer-local `go build` artifacts.
var GitSHA = ""

// ClientID is the wire-facing identifier the API server reads from
// the X-Weft-Client header. Stable format: "<binary>/<version>" with
// an optional "+<sha>" suffix on released artifacts. The server-side
// gate parses on '/' and the optional '+'.
func ClientID() string {
	if GitSHA != "" {
		return "weft/" + Version + "+" + GitSHA
	}
	return "weft/" + Version
}
