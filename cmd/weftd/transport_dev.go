//go:build weftd_dev

package main

// transportConfig is the dev-build-only override of the coordinator address
// and TLS gate, for the dockerized stack (a plaintext coordinator:9020). It
// does not exist in release builds. Build with -tags weftd_dev.
type transportConfig struct {
	Endpoint string `yaml:"endpoint"` // coordinator address; defaults to defaultEndpoint when omitted
	TLS      *bool  `yaml:"tls"`      // encrypt the coordinator connection; nil = on
}

func (t transportConfig) endpoint() string {
	if t.Endpoint == "" {
		return defaultEndpoint
	}
	return t.Endpoint
}

func (t transportConfig) tlsEnabled() bool { return t.TLS == nil || *t.TLS }
