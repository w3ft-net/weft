// Package release holds the shared client side of the binary
// distribution channel: fetching the release manifest written by
// .kevcons/ext/release.lua, picking the platform line, and
// downloading + sha256-verifying + atomically replacing a binary.
//
// Both `weft update` (cmd/weft) and the weftd self-updater
// (weftd/selfupdate.go) drive this — the only difference is who calls
// it (an interactive CLI vs. a daemon goroutine) and what they do after
// the replace (print a hint vs. restart the process).
package release

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Manifest mirrors the JSON shape .kevcons/ext/release.lua emits to
// <binary>/latest/manifest.json. The decoder tolerates extra fields so
// the release ext can grow the shape without forcing a lockstep client
// update.
type Manifest struct {
	Binary    string     `json:"binary"`
	Version   string     `json:"version"`
	SHA       string     `json:"sha"`
	Platforms []Platform `json:"platforms"`
}

type Platform struct {
	Suffix string `json:"suffix"`
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
}

// FetchManifest GETs and decodes the manifest at url.
func FetchManifest(url string) (*Manifest, error) {
	return FetchManifestWith(&http.Client{Timeout: 30 * time.Second}, url)
}

// FetchManifestWith is FetchManifest with a caller-supplied client (used
// by tests to point at an httptest server, and by the daemon to reuse
// its configured client).
func FetchManifestWith(c *http.Client, url string) (*Manifest, error) {
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var m Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// FetchSignedManifest is FetchManifestWith for a manifest whose Ed25519
// signature, base64 in the file at url+".sig", must verify against pub. The
// signature covers the manifest's bytes exactly as served, so a manifest that
// has been altered, or that was signed by another key, is refused before it is
// parsed.
func FetchSignedManifest(c *http.Client, url string, pub ed25519.PublicKey) (*Manifest, error) {
	body, err := get(c, url)
	if err != nil {
		return nil, err
	}
	sigText, err := get(c, url+".sig")
	if err != nil {
		return nil, fmt.Errorf("manifest signature: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigText)))
	if err != nil || !ed25519.Verify(pub, body, sig) {
		return nil, fmt.Errorf("manifest signature does not verify against the configured key")
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

func get(c *http.Client, url string) ([]byte, error) {
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// ParsePublicKey decodes a base64 Ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("not a base64 Ed25519 public key (%d bytes expected)", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// FindPlatform returns the manifest's platform line for goos/goarch, or
// nil when the manifest carries no build for this host.
func FindPlatform(m *Manifest, goos, goarch string) *Platform {
	for i := range m.Platforms {
		p := &m.Platforms[i]
		if p.GOOS == goos && p.GOARCH == goarch {
			return p
		}
	}
	return nil
}

// DownloadAndVerify pulls url into a temp file, verifies its sha256
// against wantHex (when non-empty), chmods it 0755, and returns the temp
// path. The caller owns removing the file. A mismatch removes the temp
// and errors — a corrupt or tampered download never reaches the install
// step.
func DownloadAndVerify(url, wantHex string) (string, error) {
	return DownloadAndVerifyWith(&http.Client{Timeout: 5 * time.Minute}, url, wantHex)
}

// DownloadAndVerifyWith is DownloadAndVerify with a caller-supplied client.
func DownloadAndVerifyWith(c *http.Client, url, wantHex string) (string, error) {
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, url)
	}

	tmp, err := os.CreateTemp("", "weft-release-*")
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

// ReplaceBinary moves src over dst. Tries os.Rename first (atomic on the
// same filesystem); falls back to a copy-through-sibling-then-rename when
// src is on a different volume than the install location.
func ReplaceBinary(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
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

// VersionNewer reports whether candidate is a strictly newer semver than
// current. Both are bare dotted versions ("0.18.0"); a leading
// "<binary>/" prefix and a trailing "+<sha>" build suffix are stripped
// so a raw ClientID ("weftd/0.18.0+abc123") compares coherently too.
//
// Part-wise compare: numeric where both sides parse as integers,
// lexicographic otherwise; a shorter version is zero-padded
// ("0.18" == "0.18.0"). Ported from coordinator/service/clientid.go,
// which can't be imported here (it's in package service, and the agent
// must not depend on coordinator internals).
func VersionNewer(candidate, current string) bool {
	return compareVersions(stripVersion(candidate), stripVersion(current)) > 0
}

func stripVersion(v string) string {
	if i := strings.Index(v, "/"); i >= 0 {
		v = v[i+1:]
	}
	if i := strings.Index(v, "+"); i >= 0 {
		v = v[:i]
	}
	return v
}

func compareVersions(a, b string) int {
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var as, bs string
		if i < len(pa) {
			as = pa[i]
		}
		if i < len(pb) {
			bs = pb[i]
		}
		if c := compareVersionPart(as, bs); c != 0 {
			return c
		}
	}
	return 0
}

func compareVersionPart(a, b string) int {
	an, aok := parseUint(a)
	bn, bok := parseUint(b)
	if aok && bok {
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		default:
			return 0
		}
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func parseUint(s string) (uint64, bool) {
	if s == "" {
		return 0, true // missing part = 0
	}
	var n uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	return n, true
}
