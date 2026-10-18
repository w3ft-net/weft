// Package weftd contains helpers used by the cmd/weftd binary. This
// file implements the daily check against the public release manifest
// at releases.w3ft.net.
package weftd

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/graybeardcorp/weft/internal/release"
	"github.com/graybeardcorp/weft/internal/weftlog"
)

// SelfUpdateChecker checks releases.w3ft.net for a newer weftd. When
// AutoApply is set (the default — see cmd/weftd autoUpdateEnabled), a
// newer build is downloaded, sha256-verified, swapped over the running
// binary, and the process restarts into it (systemd Restart=always
// relaunches). When AutoApply is off it only nags once a day so the
// operator can act on their own schedule. The download/verify/replace
// mechanics live in internal/release, shared with `weft update`.
type SelfUpdateChecker struct {
	CurrentVersion string        // the running binary's Version
	AutoApply      bool          // download+replace+restart vs. nag-only
	ManifestURL    string        // default https://releases.w3ft.net/weftd/latest/manifest.json
	CheckInterval  time.Duration // default 24h
	HTTP           *http.Client  // default 30s timeout

	// Test seams. ExecPath resolves the binary to overwrite (default
	// os.Executable); Restart hands control back to the supervisor after a
	// successful swap (default: SIGTERM self → the existing graceful
	// shutdown in cmd/weftd/main → systemd relaunch).
	ExecPath func() (string, error)
	Restart  func()
}

// init fills unset fields with their defaults. Idempotent and called
// from both Run and checkOnce, so a test can drive checkOnce directly
// (overriding only the seams it cares about) without tripping a nil
// client or nil hook.
func (c *SelfUpdateChecker) init() {
	if c.ManifestURL == "" {
		c.ManifestURL = "https://releases.w3ft.net/weftd/latest/manifest.json"
	}
	if c.CheckInterval <= 0 {
		c.CheckInterval = 24 * time.Hour
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if c.ExecPath == nil {
		c.ExecPath = os.Executable
	}
	if c.Restart == nil {
		c.Restart = func() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) }
	}
}

// Run blocks until ctx is cancelled, firing one check immediately on
// start and one every CheckInterval thereafter. Errors are logged and
// the loop keeps going — a transient network blip or a bad release
// shouldn't kill the nag (or, with AutoApply, ingest).
func (c *SelfUpdateChecker) Run(ctx context.Context) {
	c.init()

	c.checkOnce()
	t := time.NewTicker(c.CheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.checkOnce()
		}
	}
}

// checkOnce reports whether it replaced the running binary. Run ignores
// the result (its restart arrives via the Restart hook).
func (c *SelfUpdateChecker) checkOnce() bool {
	c.init()
	m, err := release.FetchManifestWith(c.HTTP, c.ManifestURL)
	if err != nil {
		weftlog.Infof(context.Background(), "selfupdate: %v", err)
		return false
	}
	if m.Version == "" || !release.VersionNewer(m.Version, c.CurrentVersion) {
		return false
	}
	// Find the platform line that matches us; without one there's
	// nothing actionable to suggest or pull.
	plat := release.FindPlatform(m, runtime.GOOS, runtime.GOARCH)
	if plat == nil {
		weftlog.Infof(context.Background(), "selfupdate: weftd %s available but no %s/%s build in manifest",
			m.Version, runtime.GOOS, runtime.GOARCH)
		return false
	}

	if !c.AutoApply {
		weftlog.Infof(context.Background(), "selfupdate: weftd %s available (current %s) — see %s",
			m.Version, c.CurrentVersion, plat.URL)
		return false
	}

	return c.apply(m, plat)
}

// apply downloads the new binary, verifies it, swaps it over the running
// executable, and restarts. Any failure is logged and returns false —
// the caller's loop retries on the next tick, so a flaky download or a
// momentarily-missing artifact never costs us ingest. Returns true only
// when the binary on disk is now the new build.
func (c *SelfUpdateChecker) apply(m *release.Manifest, plat *release.Platform) bool {
	dst, err := c.ExecPath()
	if err != nil {
		weftlog.Infof(context.Background(), "selfupdate: resolve running binary: %v", err)
		return false
	}
	// Replace the real binary, not a symlink to it.
	if real, err := filepath.EvalSymlinks(dst); err == nil {
		dst = real
	}

	weftlog.Infof(context.Background(), "selfupdate: downloading weftd %s for %s/%s", m.Version, runtime.GOOS, runtime.GOARCH)
	// Own client with a generous timeout — the manifest client's 30s is
	// fine for a few-KB JSON but tight for a multi-MB binary on a slow link.
	tmp, err := release.DownloadAndVerify(plat.URL, plat.SHA256)
	if err != nil {
		weftlog.Infof(context.Background(), "selfupdate: download weftd %s: %v", m.Version, err)
		return false
	}
	defer os.Remove(tmp)

	if err := release.ReplaceBinary(tmp, dst); err != nil {
		weftlog.Infof(context.Background(), "selfupdate: install weftd %s: %v", m.Version, err)
		return false
	}

	weftlog.Infof(context.Background(), "selfupdate: applied weftd %s → %s, restarting", c.CurrentVersion, m.Version)
	c.Restart()
	return true
}
