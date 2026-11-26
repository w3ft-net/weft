package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

// weftdVersionFile is where the agent's released version lives. Docs write
// {{WEFTD_VERSION}} wherever a release URL needs it, so a version bump flows
// into the manual without anyone editing prose. Set from the -weftd-version
// flag; the default is in the core module, wherever Go finds it (the workspace's
// checkout, the module cache, or vendor/).
var weftdVersionFile = coreWeftdVersionFile()

func coreWeftdVersionFile() string {
	out, err := exec.Command("go", "list", "-f", "{{.Dir}}", "github.com/w3ft-net/weft/cmd/weftd").Output()
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		return "cmd/weftd/version.go"
	}
	return filepath.Join(string(bytes.TrimSpace(out)), "version.go")
}

const weftdVersionSentinel = "{{WEFTD_VERSION}}"

var weftdVersionRE = regexp.MustCompile(`(?m)^var Version = "(\d+\.\d+\.\d+)"`)

func parseWeftdVersion(src []byte) (string, error) {
	m := weftdVersionRE.FindSubmatch(src)
	if m == nil {
		return "", fmt.Errorf(`no 'var Version = "x.y.z"' line found`)
	}
	return string(m[1]), nil
}

// substituteVersion replaces {{WEFTD_VERSION}} in a doc body. Docs without the
// sentinel never touch the version file, and an unresolvable version is an
// error rather than a blank in a published install command. Other sentinels
// (e.g. {{TIER_LIMITS}}, split on at runtime by the shell) pass through.
func substituteVersion(body []byte, versionFile string) ([]byte, error) {
	if !bytes.Contains(body, []byte(weftdVersionSentinel)) {
		return body, nil
	}
	src, err := os.ReadFile(versionFile)
	if err != nil {
		return nil, fmt.Errorf("%s needs the weftd version: %w", weftdVersionSentinel, err)
	}
	v, err := parseWeftdVersion(src)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", versionFile, err)
	}
	return bytes.ReplaceAll(body, []byte(weftdVersionSentinel), []byte(v)), nil
}
