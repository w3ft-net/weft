package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/graybeardcorp/weft/internal/release"
)

// runUpdate implements `weft update`. Fetches the latest manifest from
// releases.w3ft.net, compares against the embedded Version, and if newer
// downloads, verifies sha256, and atomically replaces the running
// binary. The user's current shell session keeps running the old binary
// (the OS keeps the open file mapped); next invocation picks up new.
//
// Distribution channel matches .kevcons/ext/release.lua: binaries land
// at s3://weft-releases-220664822695 fronted by releases.w3ft.net at
// /<binary>/v<version>/<binary>-<platform-suffix> plus a /latest/ alias
// and a /latest/manifest.json that the release ext writes. The
// download/verify/replace mechanics live in internal/release, shared
// with the weftd self-updater.
const (
	releaseBase = "https://releases.w3ft.net"
	manifestURL = releaseBase + "/weft/latest/manifest.json"
)

func runUpdate() {
	m, err := release.FetchManifest(manifestURL)
	if err != nil {
		errorf("fetch manifest: %v\n", err)
		os.Exit(1)
	}

	if !release.VersionNewer(m.Version, Version) {
		hintf("weft %s — already on latest\n", Version)
		return
	}

	plat := release.FindPlatform(m, runtime.GOOS, runtime.GOARCH)
	if plat == nil {
		errorf(
			"no published weft binary for %s/%s in manifest (latest=%s) — see %s\n",
			runtime.GOOS, runtime.GOARCH, m.Version, releaseBase+"/weft/latest/")
		os.Exit(1)
	}

	hintf("downloading weft %s for %s/%s ...\n", m.Version, runtime.GOOS, runtime.GOARCH)
	if err := installRelease(plat); err != nil {
		errorf("%v\n", err)
		os.Exit(1)
	}

	successf("weft updated %s → %s. Restart your shell to use the new version.\n", Version, m.Version)
}

// installRelease downloads plat's binary, verifies its sha256, and
// atomically replaces the running executable. Shared by `weft update`
// and the automatic updater.
func installRelease(plat *release.Platform) error {
	tmp, err := release.DownloadAndVerify(plat.URL, plat.SHA256)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer os.Remove(tmp)

	dst, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve running binary path: %w", err)
	}
	// Resolve symlinks so we replace the actual binary, not the link.
	if real, err := filepath.EvalSymlinks(dst); err == nil {
		dst = real
	}
	if err := release.ReplaceBinary(tmp, dst); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	return nil
}
