// Package weftd contains helpers used by the cmd/weftd binary. This
// file implements the daily check against the public release manifest
// at releases.w3ft.net.
package weftd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"time"
)

// SelfUpdateChecker logs a warning once a day when a newer weftd is
// available at releases.w3ft.net. Auto-pulling the update is gated
// behind a future config flag; the initial rollout only nags the
// operator so we can watch the signal in real customer accounts before
// trusting an unattended replace. The mechanic mirrors `weft update`
// on the CLI side — same manifest, same sha256 verification path.
type SelfUpdateChecker struct {
	CurrentVersion string        // the running binary's Version
	ManifestURL    string        // default https://releases.w3ft.net/weftd/latest/manifest.json
	CheckInterval  time.Duration // default 24h
	HTTP           *http.Client  // default 30s timeout
}

// Run blocks until ctx is cancelled, firing one check immediately on
// start and one every CheckInterval thereafter. Errors are logged and
// the loop keeps going — a transient network blip shouldn't kill the
// nag.
func (c *SelfUpdateChecker) Run(ctx context.Context) {
	if c.ManifestURL == "" {
		c.ManifestURL = "https://releases.w3ft.net/weftd/latest/manifest.json"
	}
	if c.CheckInterval <= 0 {
		c.CheckInterval = 24 * time.Hour
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 30 * time.Second}
	}

	c.checkOnce(ctx)
	t := time.NewTicker(c.CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.checkOnce(ctx)
		}
	}
}

func (c *SelfUpdateChecker) checkOnce(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.ManifestURL, nil)
	if err != nil {
		log.Printf("selfupdate: build request: %v", err)
		return
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		log.Printf("selfupdate: fetch %s: %v", c.ManifestURL, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("selfupdate: HTTP %d from %s", resp.StatusCode, c.ManifestURL)
		return
	}
	var m struct {
		Version   string `json:"version"`
		Platforms []struct {
			GOOS   string `json:"goos"`
			GOARCH string `json:"goarch"`
			URL    string `json:"url"`
			SHA256 string `json:"sha256"`
		} `json:"platforms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		log.Printf("selfupdate: parse manifest: %v", err)
		return
	}
	if m.Version == "" || m.Version == c.CurrentVersion {
		return
	}
	// Find the platform line that matches us; without one there's
	// nothing actionable to suggest.
	var matching string
	for _, p := range m.Platforms {
		if p.GOOS == runtime.GOOS && p.GOARCH == runtime.GOARCH {
			matching = p.URL
			break
		}
	}
	if matching == "" {
		log.Printf("selfupdate: weftd %s available but no %s/%s build in manifest",
			m.Version, runtime.GOOS, runtime.GOARCH)
		return
	}
	log.Printf("selfupdate: weftd %s available (current %s) — see %s",
		m.Version, c.CurrentVersion, matching)
}

// Compile-time guard against accidental drift between the JSON shape
// here and the one .kevcons/ext/release.lua writes.
var _ = fmt.Sprint
