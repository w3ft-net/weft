package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/internal/release"
)

func newTestUpdater(t *testing.T, manifestVersion string) (u *autoUpdater, installs, execs *int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(release.Manifest{
			Version:   manifestVersion,
			Platforms: []release.Platform{{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, URL: "http://x/weft"}},
		})
	}))
	t.Cleanup(srv.Close)
	installs, execs = new(int), new(int)
	return &autoUpdater{
		ManifestURL: srv.URL,
		HTTP:        srv.Client(),
		StampPath:   filepath.Join(t.TempDir(), "weft", "update-check"),
		Now:         time.Now,
		Install:     func(*release.Platform) error { *installs++; return nil },
		Exec: func(_ string, _ []string, env []string) error {
			*execs++
			for _, e := range env {
				if e == autoUpdatedEnv+"=1" {
					return nil
				}
			}
			t.Error("re-exec env missing " + autoUpdatedEnv)
			return nil
		},
	}, installs, execs
}

func TestAutoUpdateInstallsNewerAndReexecs(t *testing.T) {
	u, installs, execs := newTestUpdater(t, "0.9.0")
	u.run("0.8.1", []string{"weft", "sh"}, nil)
	if *installs != 1 || *execs != 1 {
		t.Fatalf("installs=%d execs=%d, want 1/1", *installs, *execs)
	}
	if _, err := os.Stat(u.StampPath); err != nil {
		t.Fatalf("stamp not written: %v", err)
	}
}

func TestAutoUpdateThrottledWithinInterval(t *testing.T) {
	u, installs, _ := newTestUpdater(t, "0.9.0")
	u.run("0.8.1", nil, nil)
	u.run("0.8.1", nil, nil)
	if *installs != 1 {
		t.Fatalf("installs=%d, want 1 (second run inside 24h)", *installs)
	}
	u.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	u.run("0.8.1", nil, nil)
	if *installs != 2 {
		t.Fatalf("installs=%d, want 2 after the interval elapsed", *installs)
	}
}

func TestAutoUpdateSkipsWhenCurrent(t *testing.T) {
	u, installs, _ := newTestUpdater(t, "0.8.1")
	u.run("0.8.1", nil, nil)
	if *installs != 0 {
		t.Fatalf("installs=%d, want 0 when already on latest", *installs)
	}
}

func withNotice(u *autoUpdater, t *testing.T) *bytes.Buffer {
	u.LatestPath = filepath.Join(filepath.Dir(u.StampPath), "latest-version")
	var buf bytes.Buffer
	u.Notice = &buf
	return &buf
}

// With auto-update opted out the CLI still tells you a release exists.
func TestNoticeWhenAutoUpdateDisabled(t *testing.T) {
	u, installs, _ := newTestUpdater(t, "0.9.0")
	buf := withNotice(u, t)
	u.InstallDisabled = true
	u.run("0.8.1", nil, nil)
	if *installs != 0 {
		t.Fatalf("installs=%d, want 0 when disabled", *installs)
	}
	out := buf.String()
	if !strings.Contains(out, "new version available") || !strings.Contains(out, "0.8.1 → 0.9.0") || !strings.Contains(out, "weft update") {
		t.Errorf("notice = %q", out)
	}
}

// The notice shows on every invocation, not just the one that fetched: later
// runs inside the throttle window read the cached latest version.
func TestNoticeRepeatsFromCacheWithoutFetching(t *testing.T) {
	u, _, _ := newTestUpdater(t, "0.9.0")
	buf := withNotice(u, t)
	u.InstallDisabled = true
	u.run("0.8.1", nil, nil)
	buf.Reset()
	u.ManifestURL = "http://127.0.0.1:1/unreachable"
	u.run("0.8.1", nil, nil)
	if !strings.Contains(buf.String(), "0.9.0") {
		t.Errorf("cached notice missing: %q", buf.String())
	}
}

// A failed install must not leave the user unaware an update exists.
func TestNoticeAfterFailedInstall(t *testing.T) {
	u, _, _ := newTestUpdater(t, "0.9.0")
	buf := withNotice(u, t)
	u.Install = func(*release.Platform) error { return errors.New("read-only") }
	u.run("0.8.1", nil, nil)
	if !strings.Contains(buf.String(), "weft update") {
		t.Errorf("notice = %q", buf.String())
	}
}

func TestNoNoticeWhenCurrent(t *testing.T) {
	u, _, _ := newTestUpdater(t, "0.8.1")
	buf := withNotice(u, t)
	u.InstallDisabled = true
	u.run("0.8.1", nil, nil)
	if buf.Len() != 0 {
		t.Errorf("unexpected notice: %q", buf.String())
	}
}
