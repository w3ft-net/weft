package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/graybeardcorp/weft/internal/release"
	"github.com/graybeardcorp/weft/shell"
)

const (
	autoUpdateInterval = 24 * time.Hour
	// A check runs before the user's command, so the manifest fetch gets a
	// tight budget: an unreachable release host must not stall the CLI.
	autoUpdateManifestTimeout = 2 * time.Second
	// Set across the re-exec so the new binary never checks again, which
	// bounds a manifest/binary version disagreement to a single hop.
	autoUpdatedEnv = "WEFT_AUTO_UPDATED"
)

// autoUpdater is the automatic counterpart of `weft update`: at most once
// per autoUpdateInterval it compares the running version against the
// release manifest and, when a newer build exists, installs it and
// re-execs so the invocation that triggered the check runs the new code.
type autoUpdater struct {
	ManifestURL string
	HTTP        *http.Client
	StampPath   string // mtime = last check
	Now         func() time.Time
	Install     func(*release.Platform) error
	Exec        func(argv0 string, argv []string, env []string) error
}

// maybeAutoUpdate runs the check for the current invocation. Every failure
// is swallowed (one stderr line at most): updating is a convenience and
// must never block or break the command the user actually asked for.
// origArgs is os.Args as typed, before extractTopLevelFlags strips --org, so
// the re-exec reproduces the invocation exactly.
func maybeAutoUpdate(origArgs []string) {
	// Dev builds carry no GitSHA — updating one would silently replace a
	// developer's local build with the last release.
	if GitSHA == "" || os.Getenv(autoUpdatedEnv) != "" || os.Getenv("WEFT_NO_AUTO_UPDATE") != "" {
		return
	}
	if cfg, err := shell.LoadConfig(); err == nil && cfg.AutoUpdate != nil && !*cfg.AutoUpdate {
		return
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return
	}
	u := &autoUpdater{
		ManifestURL: manifestURL,
		HTTP:        &http.Client{Timeout: autoUpdateManifestTimeout},
		StampPath:   filepath.Join(cacheDir, "weft", "update-check"),
		Now:         time.Now,
		Install:     installRelease,
		Exec:        syscall.Exec,
	}
	u.run(Version, origArgs, os.Environ())
}

func (u *autoUpdater) run(current string, argv, env []string) {
	if !u.due() {
		return
	}
	// Stamp before fetching so an offline machine, a read-only install
	// location, or a broken release costs one attempt per interval, not
	// one per command.
	u.stamp()

	m, err := release.FetchManifestWith(u.HTTP, u.ManifestURL)
	if err != nil || !release.VersionNewer(m.Version, current) {
		return
	}
	plat := release.FindPlatform(m, runtime.GOOS, runtime.GOARCH)
	if plat == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "weft: updating %s → %s ...\n", current, m.Version)
	if err := u.Install(plat); err != nil {
		fmt.Fprintf(os.Stderr, "weft: auto-update failed (%v); run `weft update` to retry\n", err)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if err := u.Exec(exe, argv, append(env, autoUpdatedEnv+"=1")); err != nil {
		fmt.Fprintf(os.Stderr, "weft: updated to %s; re-run your command to use it\n", m.Version)
		os.Exit(0)
	}
}

func (u *autoUpdater) due() bool {
	fi, err := os.Stat(u.StampPath)
	return err != nil || u.Now().Sub(fi.ModTime()) >= autoUpdateInterval
}

func (u *autoUpdater) stamp() {
	if err := os.MkdirAll(filepath.Dir(u.StampPath), 0o755); err != nil {
		return
	}
	now := u.Now()
	if err := os.WriteFile(u.StampPath, nil, 0o644); err != nil {
		return
	}
	_ = os.Chtimes(u.StampPath, now, now)
}
