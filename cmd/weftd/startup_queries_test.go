package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// deploy/startup.sh tails the coordinator's query-audit log as its own
// source so it lands at /log/weft/coordinator/queries.log (kind=queries)
// rather than folding into coordinator/main.log. Under weftd 0.20.0 the
// source's name comes from `service:`; a stale `app:` key is silently
// ignored, leaving the service blank so weftd falls back to
// basename(path) → "coordinator-queries", landing the stream at the
// wrong pseudofile (/log/weft/coordinator-queries/...).
//
// This parses the actual QUERIES_SOURCE snippet out of startup.sh and
// pins that it resolves to service "coordinator", kind "queries".
func TestStartupQueriesSourceService(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	startup := filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "startup.sh")
	data, err := os.ReadFile(startup)
	if err != nil {
		t.Fatalf("read startup.sh: %v", err)
	}

	snippet, ok := extractShellString(string(data), `QUERIES_SOURCE="`)
	if !ok {
		t.Fatal(`could not find QUERIES_SOURCE="..." assignment in startup.sh`)
	}

	// The snippet is a single YAML list item ("  - path: ...\n ...").
	var cfg Config
	if err := yaml.Unmarshal([]byte("sources:\n"+snippet), &cfg); err != nil {
		t.Fatalf("QUERIES_SOURCE snippet does not parse as a weftd source: %v\n%s", err, snippet)
	}
	if len(cfg.Sources) != 1 {
		t.Fatalf("expected 1 source from QUERIES_SOURCE, got %d", len(cfg.Sources))
	}
	src := cfg.Sources[0]
	if src.Service != "coordinator" {
		t.Errorf("QUERIES_SOURCE Service = %q, want \"coordinator\" (still on `app:`? it renders as basename %q)",
			src.Service, basenameService(src.Path))
	}
	if src.Kind != "queries" {
		t.Errorf("QUERIES_SOURCE Kind = %q, want \"queries\"", src.Kind)
	}
	if !strings.HasSuffix(src.Path, "coordinator-queries.log") {
		t.Errorf("QUERIES_SOURCE Path = %q, want it to tail coordinator-queries.log", src.Path)
	}
}

// extractShellString returns the body of the first non-empty
// double-quoted shell assignment that begins with marker (e.g. `FOO="`),
// up to the next double quote. Skips empty initializers like FOO="".
// Good enough for our quote-free YAML snippets.
func extractShellString(content, marker string) (string, bool) {
	for {
		i := strings.Index(content, marker)
		if i < 0 {
			return "", false
		}
		rest := content[i+len(marker):]
		j := strings.IndexByte(rest, '"')
		if j < 0 {
			return "", false
		}
		if body := rest[:j]; body != "" {
			return body, true
		}
		content = rest[j+1:] // empty body (e.g. FOO=""); keep looking
	}
}
