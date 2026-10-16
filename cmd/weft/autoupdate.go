package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/graybeardcorp/weft/internal/release"
	"github.com/graybeardcorp/weft/shell"
	"golang.org/x/term"
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

	// InstallDisabled turns the updater into a notifier: it still learns the
	// latest version, but leaves installing to `weft update`.
	InstallDisabled bool
	// LatestPath caches the newest version seen, so the notice can show on
	// every invocation while the manifest is fetched at most daily.
	LatestPath string
	// Notice, when set, receives a "new version available" line.
	Notice io.Writer
}

// maybeAutoUpdate runs the check for the current invocation. Every failure
// is swallowed (one stderr line at most): updating is a convenience and
// must never block or break the command the user actually asked for.
// origArgs is os.Args as typed, before extractTopLevelFlags strips --org, so
// the re-exec reproduces the invocation exactly.
func maybeAutoUpdate(origArgs []string) {
	// Dev builds carry no GitSHA — updating one would silently replace a
	// developer's local build with the last release.
	if GitSHA == "" {
		return
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return
	}
	disabled := os.Getenv("WEFT_NO_AUTO_UPDATE") != ""
	if cfg, err := shell.LoadConfig(); err == nil && cfg.AutoUpdate != nil && !*cfg.AutoUpdate {
		disabled = true
	}
	u := &autoUpdater{
		ManifestURL:     manifestURL,
		HTTP:            &http.Client{Timeout: autoUpdateManifestTimeout},
		StampPath:       filepath.Join(cacheDir, "weft", "update-check"),
		LatestPath:      filepath.Join(cacheDir, "weft", "latest-version"),
		Now:             time.Now,
		Install:         installRelease,
		Exec:            syscall.Exec,
		InstallDisabled: disabled,
	}
	// The notice is for people: stderr only when it is a terminal, and never
	// on the MCP stdio server.
	if len(os.Args) > 1 && os.Args[1] != "mcp" && term.IsTerminal(int(os.Stderr.Fd())) {
		u.Notice = os.Stderr
	}
	if os.Getenv(autoUpdatedEnv) != "" {
		// Just re-exec'd by an update: nothing to fetch or install, but a
		// still-newer cached version is worth mentioning.
		u.notice(Version)
		return
	}
	u.run(Version, origArgs, os.Environ())
}

func (u *autoUpdater) run(current string, argv, env []string) {
	defer u.notice(current)
	if !u.due() {
		return
	}
	// Stamp before fetching so an offline machine, a read-only install
	// location, or a broken release costs one attempt per interval, not
	// one per command.
	u.stamp()

	m, err := release.FetchManifestWith(u.HTTP, u.ManifestURL)
	if err != nil {
		return
	}
	u.saveLatest(m.Version)
	if u.InstallDisabled || !release.VersionNewer(m.Version, current) {
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

func (u *autoUpdater) saveLatest(v string) {
	if u.LatestPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(u.LatestPath), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(u.LatestPath, []byte(v), 0o644)
}

// notice tells the user a newer release exists, from the cached latest
// version (so it costs no network on most invocations).
func (u *autoUpdater) notice(current string) {
	if u.Notice == nil || u.LatestPath == "" {
		return
	}
	b, err := os.ReadFile(u.LatestPath)
	if err != nil {
		return
	}
	latest := strings.TrimSpace(string(b))
	if latest == "" || !release.VersionNewer(latest, current) {
		return
	}
	st := newStyles(u.Notice)
	fmt.Fprintf(u.Notice, "%s %s\n", st.warn.Render(fmt.Sprintf("new version available (%s → %s),", current, latest)),
		"run "+st.name.Render("`weft update`")+" to upgrade")
}
