package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
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
// and a /latest/manifest.json that the release ext writes.
func runUpdate() {
	const releaseBase = "https://releases.w3ft.net"
	manifestURL := releaseBase + "/weft/latest/manifest.json"

	m, err := fetchManifest(manifestURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch manifest: %v\n", err)
		os.Exit(1)
	}

	if m.Version == Version {
		fmt.Printf("weft %s — already on latest\n", Version)
		return
	}

	plat := findPlatform(m, runtime.GOOS, runtime.GOARCH)
	if plat == nil {
		fmt.Fprintf(os.Stderr,
			"no published weft binary for %s/%s in manifest (latest=%s) — see %s\n",
			runtime.GOOS, runtime.GOARCH, m.Version, releaseBase+"/weft/latest/")
		os.Exit(1)
	}

	fmt.Printf("downloading weft %s for %s/%s ...\n", m.Version, runtime.GOOS, runtime.GOARCH)
	tmp, err := downloadAndVerify(plat.URL, plat.SHA256)
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

	if err := replaceBinary(tmp, dst); err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("weft updated %s → %s. Restart your shell to use the new version.\n", Version, m.Version)
}

// updateManifest mirrors the JSON shape .kevcons/ext/release.lua emits.
// Tagged here so the parser tolerates additional fields the release ext
// may grow without forcing a CLI update in lockstep.
type updateManifest struct {
	Binary    string             `json:"binary"`
	Version   string             `json:"version"`
	SHA       string             `json:"sha"`
	Platforms []manifestPlatform `json:"platforms"`
}

type manifestPlatform struct {
	Suffix string `json:"suffix"`
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
}

func fetchManifest(url string) (*updateManifest, error) {
	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var m updateManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

func findPlatform(m *updateManifest, goos, goarch string) *manifestPlatform {
	for i := range m.Platforms {
		p := &m.Platforms[i]
		if p.GOOS == goos && p.GOARCH == goarch {
			return p
		}
	}
	return nil
}

// downloadAndVerify pulls the URL into a sibling temp file of the
// destination (so a same-filesystem rename succeeds), verifies the
// sha256, and returns the temp path. Caller is responsible for removing
// the file on early returns.
func downloadAndVerify(url, wantHex string) (string, error) {
	c := &http.Client{Timeout: 5 * time.Minute}
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, url)
	}

	tmp, err := os.CreateTemp("", "weft-update-*")
	if err != nil {
		return "", err
	}
	defer tmp.Close()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	gotHex := hex.EncodeToString(hasher.Sum(nil))
	if wantHex != "" && gotHex != wantHex {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("sha256 mismatch: got %s, want %s", gotHex, wantHex)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// replaceBinary moves src over dst. Tries os.Rename first (atomic on
// same fs); falls back to a copy when the source temp is on a different
// volume than the install location.
func replaceBinary(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// Cross-device — copy through a sibling temp, then atomically rename.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	sibling := dst + ".weft-update-tmp"
	out, err := os.OpenFile(sibling, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(sibling)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(sibling)
		return err
	}
	if err := os.Rename(sibling, dst); err != nil {
		os.Remove(sibling)
		return err
	}
	return nil
}
