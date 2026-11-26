package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const legacyYAML = `app: svc
format: text
timestamp_field: "1"
timestamp_fields: 2
timestamp_format: "2006-01-02 15:04:05,000"
level_field: "3"
`

func manyLines(n int, line string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = line
	}
	return out
}

func TestPlanMigrationSafeWhenTheReplayAgrees(t *testing.T) {
	lines := manyLines(30, "[2026-10-15 12:00:01,123: INFO] hello")
	p, err := planMigration(legacyYAML, lines)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Safe() || p.Converted != 2 {
		t.Fatalf("plan = %+v, want safe with the timestamp and level converted", p)
	}
	if !strings.Contains(p.NewYAML, "extract:") {
		t.Errorf("new schema has no extract section:\n%s", p.NewYAML)
	}
	rep := p.Report("ns/svc")
	for _, want := range []string{"ns/svc", "30 lines", "identical", "timestamp", "level"} {
		if !strings.Contains(rep, want) {
			t.Errorf("report missing %q:\n%s", want, rep)
		}
	}
}

// A difference on any line makes the plan unsafe, and the report shows it.
func TestPlanMigrationUnsafeWhenLinesDiffer(t *testing.T) {
	y := legacyYAML + "level_pattern: '^(\\w+)/'\n"
	lines := manyLines(30, "[2026-10-15 12:00:01,123: INFO/Main] hello")
	lines[7] = "[2026-10-15 12:00:02,456: WARNING] no delimiter"
	p, err := planMigration(y, lines)
	if err != nil {
		t.Fatal(err)
	}
	if p.Safe() || len(p.Diffs) != 1 {
		t.Fatalf("plan = %+v, want one difference and not safe", p)
	}
	if rep := p.Report("ns/svc"); !strings.Contains(rep, "line 8") || !strings.Contains(rep, "WARNING") {
		t.Errorf("report does not show the differing line:\n%s", rep)
	}
}

func TestPlanMigrationNeedsEnoughLines(t *testing.T) {
	p, err := planMigration(legacyYAML, manyLines(3, "[2026-10-15 12:00:01,123: INFO] hello"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Safe() {
		t.Error("three lines proved nothing, yet the plan is safe")
	}
	if !strings.Contains(p.Report("ns/svc"), "at least") {
		t.Errorf("report should say how many lines are needed:\n%s", p.Report("ns/svc"))
	}
}

func TestPlanMigrationBlockedByEnvelope(t *testing.T) {
	p, err := planMigration("app: svc\nformat: text\nenvelope: kevcons\nlevel_field: \"1\"\n", manyLines(30, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Safe() || !p.Blocked {
		t.Fatalf("plan = %+v, want blocked", p)
	}
}

func TestPlanMigrationNothingToDo(t *testing.T) {
	p, err := planMigration("app: svc\nformat: text\nlevel_field: .level\n", manyLines(30, `{"level":"info"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !p.NothingToDo() {
		t.Errorf("a JSON-path schema has nothing implicit to write down: %+v", p)
	}
}

// Applying keeps the old schema first, somewhere the caller chose.
func TestBackupSchemaWritesTheOldOne(t *testing.T) {
	dir := t.TempDir()
	path, err := backupSchema(dir, "ns", "svc", legacyYAML)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != legacyYAML {
		t.Fatalf("backup = %q, %v", got, err)
	}
	if filepath.Dir(path) != dir || !strings.Contains(filepath.Base(path), "ns.svc.") {
		t.Errorf("backup path = %s", path)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("backup mode = %v, want 0600 (a schema is account config)", info.Mode().Perm())
	}
	if _, err := backupSchema("", "ns", "svc", legacyYAML); err == nil {
		t.Error("an empty backup directory must be refused, not defaulted into the current one")
	}
	if _, err := backupSchema(filepath.Join(dir, "nope"), "ns", "svc", legacyYAML); err == nil {
		t.Error("a missing backup directory must be refused, not created")
	}
}
