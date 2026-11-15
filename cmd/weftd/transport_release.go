//go:build !weftd_dev

package main

// transportConfig carries no fields in release builds: `endpoint:` and `tls:`
// in a config file are ignored (weftd's YAML parse is lenient), so a host can
// neither be pointed away from the production coordinator nor downgraded to
// plaintext. Configs written when those keys existed keep working.
type transportConfig struct{}

func (transportConfig) endpoint() string { return defaultEndpoint }
func (transportConfig) tlsEnabled() bool { return true }
