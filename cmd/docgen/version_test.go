package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestParseWeftdVersion(t *testing.T) {
	got, err := parseWeftdVersion([]byte("package main\n\nvar Version = \"0.31.0\"\n"))
	if err != nil || got != "0.31.0" {
		t.Fatalf("parseWeftdVersion = %q, %v; want 0.31.0", got, err)
	}
	if _, err := parseWeftdVersion([]byte("package main\n")); err == nil {
		t.Fatal("a file with no Version must be an error, not an empty string")
	}
}

func TestSubstituteWeftdVersion(t *testing.T) {
	dir := t.TempDir()
	vf := filepath.Join(dir, "version.go")
	if err := os.WriteFile(vf, []byte("var Version = \"1.2.3\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := substituteVersion([]byte("v{{WEFTD_VERSION}} and {{TIER_LIMITS}}"), vf)
	if err != nil {
		t.Fatal(err)
	}
	// Other sentinels (the shell splits on {{TIER_LIMITS}} at runtime) survive.
	if string(got) != "v1.2.3 and {{TIER_LIMITS}}" {
		t.Fatalf("got %q", got)
	}

	// A doc with no placeholder never needs the version file.
	if got, err := substituteVersion([]byte("plain"), filepath.Join(dir, "missing.go")); err != nil || string(got) != "plain" {
		t.Fatalf("no-placeholder doc: %q, %v", got, err)
	}

	// A placeholder with no resolvable version fails the build.
	if _, err := substituteVersion([]byte("{{WEFTD_VERSION}}"), filepath.Join(dir, "missing.go")); err == nil {
		t.Fatal("placeholder with unreadable version file must error")
	}
}

// The manual's install command must name a deb that the release ext publishes
// (weftd/v<ver>/weftd_<ver>_<arch>.deb). There is no "latest" deb alias.
func TestInstallWeftdPointsAtVersionedDeb(t *testing.T) {
	docs, err := loadDir("../../docs/manual")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(weftdVersionFile)
	if err != nil {
		t.Fatal(err)
	}
	ver := regexp.MustCompile(`var Version = "([^"]+)"`).FindSubmatch(raw)[1]

	for _, d := range docs {
		if d.ID != "manual-weftd" {
			continue
		}
		body := string(d.body)
		want := "https://releases.w3ft.net/weftd/v" + string(ver) + "/weftd_" + string(ver) + "_amd64.deb"
		if !strings.Contains(body, want) {
			t.Errorf("install doc does not contain %s", want)
		}
		if strings.Contains(body, "{{WEFTD_VERSION}}") || strings.Contains(body, "weftd_latest") {
			t.Error("install doc still has an unresolved placeholder or a latest-deb URL")
		}
		return
	}
	t.Fatal("manual-weftd not found")
}
