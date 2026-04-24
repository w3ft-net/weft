// Coverage-boundary helpers for integration tests.
//
// The docker-compose.cover.yml overlay bind-mounts ./coverage/<service>
// into every server container with GOCOVERDIR set and builds the
// binaries with `-cover -coverpkg=./...`. After a SIGTERM-and-flush
// cycle, scripts/coverage-report.sh writes coverage/<service>.out —
// a Go cover "textfmt" profile — per container.
//
// This file exposes a loader + RequireRanIn assertion so the
// integration suite can prove a function actually executed in the
// process we expected. That matters when we're moving compute
// boundaries around: "did QueryToCompute run in the shard binary
// (not the coordinator)?" catches the kind of mock-in-the-wrong-
// process regression that's a nightmare to debug in prod.
package integration

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// CoverageEntry is one line of `go tool cover -func` output, already
// parsed. Percent is 0..100.
type CoverageEntry struct {
	File     string  // e.g. github.com/kevlarcon/weft/shard/service/server.go
	Line     int     // declaration line
	Function string  // e.g. QueryToCompute
	Percent  float64 // 0..100
}

// LoadServiceCoverage reads coverage/<service>.out (the textfmt file
// emitted by scripts/coverage-report.sh) via `go tool cover -func`
// and returns one entry per function. Returns (nil, nil) when the
// profile is absent so callers that only enforce coverage under a CI
// flag can skip cleanly.
func LoadServiceCoverage(service string) ([]CoverageEntry, error) {
	path := filepath.Join("..", "coverage", service+".out")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}

	out, err := exec.Command("go", "tool", "cover", "-func="+path).Output()
	if err != nil {
		return nil, fmt.Errorf("go tool cover -func=%s: %w", path, err)
	}

	var entries []CoverageEntry
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "total:") {
			continue
		}
		e, ok := parseFuncLine(line)
		if !ok {
			continue
		}
		entries = append(entries, e)
	}
	return entries, sc.Err()
}

// RequireRanIn fails the test unless the named function in file
// executed at least once in service's coverage profile. Skips
// (not fails) when the profile is absent — so suites that can run
// without coverage instrumentation stay green.
//
// file is matched by suffix so callers can pass a repo-relative path
// like "shard/service/server.go" without the full module prefix.
func RequireRanIn(t *testing.T, service, file, function string) {
	t.Helper()

	entries, err := LoadServiceCoverage(service)
	if err != nil {
		t.Fatalf("coverage boundary check: load %s: %v", service, err)
	}
	if entries == nil {
		t.Skipf("coverage boundary check skipped: coverage/%s.out missing (run under docker-compose.cover.yml + scripts/coverage-report.sh)", service)
		return
	}

	for _, e := range entries {
		if e.Function != function {
			continue
		}
		if !strings.HasSuffix(e.File, file) {
			continue
		}
		if e.Percent > 0 {
			return
		}
	}
	t.Fatalf(
		"coverage boundary check failed: expected %s:%s to execute in service %q but found no positive coverage.\n"+
			"Profile: coverage/%s.out",
		file, function, service, service,
	)
}

// parseFuncLine parses one line of `go tool cover -func` output:
//
//	path/to/file.go:LINE:	Func	PCT%
//
// Whitespace between columns is tab-heavy; strings.Fields tolerates
// any run of whitespace so this stays robust across Go versions.
func parseFuncLine(line string) (CoverageEntry, bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return CoverageEntry{}, false
	}

	loc := strings.TrimSuffix(fields[0], ":")
	colon := strings.LastIndex(loc, ":")
	if colon < 0 {
		return CoverageEntry{}, false
	}
	lineNo, err := strconv.Atoi(loc[colon+1:])
	if err != nil {
		return CoverageEntry{}, false
	}

	pct, err := strconv.ParseFloat(strings.TrimSuffix(fields[2], "%"), 64)
	if err != nil {
		return CoverageEntry{}, false
	}

	return CoverageEntry{
		File:     loc[:colon],
		Line:     lineNo,
		Function: fields[1],
		Percent:  pct,
	}, true
}
