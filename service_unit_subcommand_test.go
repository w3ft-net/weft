package weft_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A systemd unit may run a subcommand (`weft-shard export --db=...`), whose
// flags live on its own flag.NewFlagSet, not on the top-level flag package.
// TestServiceUnitFlagsAreDefined checks them against that subcommand's FlagSet
// alone: the binary parses them there, so a flag defined only for the
// subcommand must not satisfy a top-level use, and a typo on the subcommand
// must still fail the check.

const subcommandFixtureMain = `package main

import "flag"

func main() {
	_ = flag.Int("grpc-port", 9000, "")
	_ = flag.String("data-dir", "", "")
}
`

const subcommandFixtureExport = `package main

import (
	"flag"
	"time"
)

func runExportCommand(args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	_ = fs.String("shard-id", "", "")
	var db string
	fs.StringVar(&db, "db", "", "")
	_ = fs.Int("attempt", 0, "")
	_ = fs.Duration("timeout", time.Hour, "")
	return 0
}

// A second FlagSet in the same package must not leak into export's.
func runOtherCommand(args []string) int {
	fs := flag.NewFlagSet("other", flag.ContinueOnError)
	_ = fs.String("only-other", "", "")
	return 0
}
`

func writeSubcommandFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range map[string]string{"main.go": subcommandFixtureMain, "export.go": subcommandFixtureExport} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func writeUnit(t *testing.T, execStart string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "weft-fixture.service")
	if err := os.WriteFile(p, []byte("[Service]\nExecStart="+execStart+"\nRestart=always\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseSystemdExecStart_SeparatesTheSubcommand(t *testing.T) {
	unit := writeUnit(t, "/opt/app/bin/weft-shard export \\\n    --shard-id=${ID} \\\n    --attempt=${N}")
	bin, subcmd, flags, err := parseSystemdExecStart(unit)
	if err != nil {
		t.Fatal(err)
	}
	if bin != "/opt/app/bin/weft-shard" || subcmd != "export" || len(flags) != 2 || flags[0] != "shard-id" || flags[1] != "attempt" {
		t.Fatalf("bin=%q subcmd=%q flags=%v", bin, subcmd, flags)
	}

	// No positional: no subcommand, and the existing units' shape is unchanged.
	unit = writeUnit(t, "/opt/app/bin/weft-shard --grpc-port=9000")
	if _, subcmd, flags, _ = parseSystemdExecStart(unit); subcmd != "" || len(flags) != 1 {
		t.Fatalf("a unit with no subcommand parsed as subcmd=%q flags=%v", subcmd, flags)
	}
}

func TestUnitFlagProblems_ScopedToTheSubcommand(t *testing.T) {
	pkg := writeSubcommandFixture(t)

	cases := []struct {
		name    string
		subcmd  string
		flags   []string
		wantBad []string // flags expected to be reported
	}{
		{"every export flag defined", "export", []string{"shard-id", "db", "attempt", "timeout"}, nil},
		{"a typo on the subcommand still fails", "export", []string{"shard-id", "attemps"}, []string{"attemps"}},
		{"a top-level flag is not an export flag", "export", []string{"grpc-port"}, []string{"grpc-port"}},
		{"another subcommand's flag does not leak", "export", []string{"only-other"}, []string{"only-other"}},
		{"an export flag is not a top-level flag", "", []string{"db"}, []string{"db"}},
		{"top-level flags still check as before", "", []string{"grpc-port", "data-dir"}, nil},
		{"an unknown subcommand falls back to the top level", "nonesuch", []string{"grpc-port"}, nil},
	}
	for _, c := range cases {
		got, err := unitFlagProblems(c.subcmd, c.flags, pkg)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != len(c.wantBad) {
			t.Errorf("%s: problems = %v, want flags %v", c.name, got, c.wantBad)
			continue
		}
		for i, w := range c.wantBad {
			if !strings.Contains(got[i], "--"+w) {
				t.Errorf("%s: problem %d = %q, want it to name --%s", c.name, i, got[i], w)
			}
		}
	}
}
