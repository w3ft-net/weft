package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
