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
func runUpdate() {
	const releaseBase = "https://releases.w3ft.net"
	manifestURL := releaseBase + "/weft/latest/manifest.json"

	m, err := release.FetchManifest(manifestURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch manifest: %v\n", err)
		os.Exit(1)
	}

	if !release.VersionNewer(m.Version, Version) {
		fmt.Printf("weft %s — already on latest\n", Version)
		return
	}

	plat := release.FindPlatform(m, runtime.GOOS, runtime.GOARCH)
	if plat == nil {
		fmt.Fprintf(os.Stderr,
			"no published weft binary for %s/%s in manifest (latest=%s) — see %s\n",
			runtime.GOOS, runtime.GOARCH, m.Version, releaseBase+"/weft/latest/")
		os.Exit(1)
	}

	fmt.Printf("downloading weft %s for %s/%s ...\n", m.Version, runtime.GOOS, runtime.GOARCH)
	tmp, err := release.DownloadAndVerify(plat.URL, plat.SHA256)
	if err != nil {
		fmt.Fprintf(os.Stderr, "download: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(tmp)

	dst, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve running binary path: %v\n", err)
		os.Exit(1)
	}
	// Resolve symlinks so we replace the actual binary, not the link.
	if real, err := filepath.EvalSymlinks(dst); err == nil {
		dst = real
	}

	if err := release.ReplaceBinary(tmp, dst); err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("weft updated %s → %s. Restart your shell to use the new version.\n", Version, m.Version)
}
