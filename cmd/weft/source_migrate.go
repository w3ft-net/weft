package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/graybeardcorp/weft/ingest"
	"github.com/graybeardcorp/weft/shell"
)

// minMigrationLines is how many lines the replay needs before "identical"
// means anything. A handful of lines can agree by luck.
const minMigrationLines = 20

// migrationPlan is what rewriting one source's stored schema to the explicit
// form would do, and whether replaying real lines through both says it is safe.
type migrationPlan struct {
	Old, New  *ingest.Schema
	NewYAML   string
	Notes     []ingest.MigrationNote
	Diffs     []ingest.RowDiff
	Lines     int
	Converted int
	Blocked   bool
}

// planMigration converts a stored schema and replays lines through both forms.
func planMigration(oldYAML string, lines []string) (*migrationPlan, error) {
	old, err := ingest.LoadSchemaFromBytes([]byte(oldYAML))
	if err != nil {
		return nil, fmt.Errorf("parse stored schema: %w", err)
	}
	migrated, notes := ingest.MigrateLegacy(old)
	p := &migrationPlan{Old: old, New: migrated, Notes: notes, Lines: len(lines)}
	for _, n := range notes {
		switch n.Kind {
		case ingest.MigrationConverted:
			p.Converted++
		case ingest.MigrationBlocked:
			p.Blocked = true
		}
	}
	if p.Blocked {
		return p, nil
	}
	if p.NewYAML, err = migrated.ToYAML(); err != nil {
		return nil, err
	}
	if p.Diffs, err = ingest.CompareSchemas(old, migrated, lines); err != nil {
		return nil, err
	}
	return p, nil
}

// NothingToDo: there is no implicit reading to write down.
func (p *migrationPlan) NothingToDo() bool { return !p.Blocked && p.Converted == 0 }

// Safe: the conversion is possible, and enough real lines read identically
// under both schemas.
func (p *migrationPlan) Safe() bool {
	return !p.Blocked && p.Converted > 0 && len(p.Diffs) == 0 && p.Lines >= minMigrationLines
}

// Report is the human-readable verdict.
func (p *migrationPlan) Report(source string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", source)
	for _, n := range p.Notes {
		fmt.Fprintf(&b, "  %-15s %-9s %s\n", n.Value, map[ingest.MigrationKind]string{
			ingest.MigrationKept: "kept", ingest.MigrationConverted: "converted", ingest.MigrationBlocked: "BLOCKED",
		}[n.Kind], n.Why)
	}
	switch {
	case p.Blocked:
		b.WriteString("\ncannot be rewritten; nothing was changed\n")
	case p.NothingToDo():
		b.WriteString("\nalready explicit; nothing to do\n")
	case p.Lines < minMigrationLines:
		fmt.Fprintf(&b, "\nreplayed %d lines: not enough to trust a comparison; give at least %d\n", p.Lines, minMigrationLines)
	case len(p.Diffs) == 0:
		fmt.Fprintf(&b, "\nreplayed %d lines through both: identical\n", p.Lines)
	default:
		fmt.Fprintf(&b, "\nreplayed %d lines through both: %d value(s) differ (the explicit form is NOT equivalent)\n", p.Lines, len(p.Diffs))
		for i, d := range p.Diffs {
			if i == 10 {
				fmt.Fprintf(&b, "  ... and %d more\n", len(p.Diffs)-10)
				break
			}
			fmt.Fprintf(&b, "  line %d %s: %q -> %q\n", d.Line+1, d.Value, d.Old, d.New)
		}
		b.WriteString("re-run the wizard for this source instead\n")
	}
	return b.String()
}

// backupSchema saves a source's current schema before it is replaced. The
// directory must exist and be named: a schema is account configuration, so it
// is never defaulted into the current directory, where it could end up in git.
func backupSchema(dir, namespace, service, yamlText string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("--backup-dir is required: the old schema is saved there before anything is replaced (keep it out of git)")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("backup directory %q does not exist", dir)
	}
	path := filepath.Join(dir, fmt.Sprintf("%s.%s.%s.yaml", namespace, service, time.Now().UTC().Format("20060102T150405Z")))
	if err := os.WriteFile(path, []byte(yamlText), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// readLines reads raw log lines, one per line, from a file or "-" for stdin.
func readLines(path string) ([]string, error) {
	f := os.Stdin
	if path != "-" {
		var err error
		if f, err = os.Open(path); err != nil {
			return nil, err
		}
		defer f.Close()
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if line := strings.TrimRight(sc.Text(), "\r"); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, sc.Err()
}

// runSourceMigrate: weft source migrate NS/SERVICE --lines FILE [--apply --backup-dir DIR]
//
// Rewrites a source's legacy-form schema to the explicit one, after replaying
// real lines through both to prove they read the same. Without --apply it only
// reports. With it, the old schema is saved to --backup-dir first and the new
// one goes through the normal configure path (so the agent-version guard
// applies).
func runSourceMigrate(cfg *shell.Config) {
	usage := "usage: weft source migrate <namespace>/<service> --lines FILE|- [--apply --backup-dir DIR]\n" +
		"  produce LINES with: weft sh -c 'tail -n 500 /log/<ns>/<service>/<kind>.log' > lines.txt"
	if len(os.Args) < 4 {
		errorln(usage)
		os.Exit(1)
	}
	parts := strings.SplitN(os.Args[3], "/", 2)
	if len(parts) != 2 {
		errorln(usage)
		os.Exit(1)
	}
	namespace, service := parts[0], parts[1]
	var linesPath, backupDir string
	apply := false
	for i := 4; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--lines":
			i++
			if i < len(os.Args) {
				linesPath = os.Args[i]
			}
		case "--backup-dir":
			i++
			if i < len(os.Args) {
				backupDir = os.Args[i]
			}
		case "--apply":
			apply = true
		}
	}
	if linesPath == "" {
		errorln(usage)
		os.Exit(1)
	}
	if !strings.HasPrefix(cfg.Endpoint, "http") {
		errorln("source migrate requires HTTP endpoint")
		os.Exit(1)
	}
	remote := shell.NewHTTPCoordinator(cfg.Endpoint, cfg.Token, effectiveOrg(cfg))

	sch, err := remote.GetSourceSchema(namespace, service)
	if err != nil {
		errorf("schema: %v\n", err)
		os.Exit(1)
	}
	if sch.Status != "configured" || sch.SchemaYAML == "" {
		errorf("%s/%s has no stored schema to migrate (status %s); use `weft source configure`\n", namespace, service, sch.Status)
		os.Exit(1)
	}
	lines, err := readLines(linesPath)
	if err != nil {
		errorf("read lines: %v\n", err)
		os.Exit(1)
	}
	plan, err := planMigration(sch.SchemaYAML, lines)
	if err != nil {
		errorf("plan: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(plan.Report(namespace + "/" + service))

	if !apply {
		if plan.Safe() {
			fmt.Println("\nsafe to apply: re-run with --apply --backup-dir DIR")
		}
		return
	}
	if !plan.Safe() {
		errorln("\nnot applying: the plan is not safe")
		os.Exit(1)
	}
	saved, err := backupSchema(backupDir, namespace, service, sch.SchemaYAML)
	if err != nil {
		errorf("\nnot applying: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\nold schema saved to %s\n", saved)
	if err := remote.ConfigureSource(namespace, service, schemaConfigMap(plan.New)); err != nil {
		errorf("configure: %v\n(the old schema is unchanged; the backup is still at %s)\n", err, saved)
		os.Exit(1)
	}
	fmt.Printf("%s/%s now uses the explicit schema; roll back with: weft source configure %s/%s --file %s\n", namespace, service, namespace, service, saved)
}
