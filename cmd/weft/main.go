package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kevlarcon/weft/coordinator"
	"github.com/kevlarcon/weft/ingest"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/shell"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			runSetup()
			return
		case "ingest":
			runIngest()
			return
		}
	}

	// Load config.
	cfg, err := shell.LoadConfig()
	if err != nil {
		// Fall back to dev mode with fixtures.
		fmt.Fprintln(os.Stderr, dimText("no .weft.yaml found, using dev fixtures"))
		runDevShell()
		return
	}

	// Connect to remote coordinator.
	remote, err := shell.NewRemoteCoordinator(cfg.Endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect to %s: %v\n", cfg.Endpoint, err)
		os.Exit(1)
	}
	defer remote.Close()

	apps, err := remote.ListApps()
	if err != nil {
		fmt.Fprintf(os.Stderr, "list apps: %v\n", err)
		os.Exit(1)
	}

	s := &shell.Shell{
		Query: func(pipeline string, start, end time.Time) (*shell.QueryResult, error) {
			r, err := remote.Query(pipeline, start, end)
			if err != nil {
				return nil, err
			}
			return &shell.QueryResult{Rows: r.Rows, Text: r.Text}, nil
		},
		Start: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
		Apps:  apps,
	}

	runShell(s)
}

func runDevShell() {
	coord, cleanup, err := coordinator.NewDevCoordinator()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start: %v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	s := &shell.Shell{
		Query: func(pipeline string, start, end time.Time) (*shell.QueryResult, error) {
			tr := coordinator.TimeRange{Start: start, End: end}
			r, err := coord.Query(pipeline, tr)
			if err != nil {
				return nil, err
			}
			return &shell.QueryResult{Rows: r.Rows, Text: r.Text}, nil
		},
		Start: coordinator.DevT0,
		End:   coordinator.DevT2,
		Apps:  coordinator.DevApps(),
	}

	runShell(s)
}

func runShell(s *shell.Shell) {
	// If stdin is not a terminal, run non-interactively.
	if fi, _ := os.Stdin.Stat(); fi.Mode()&os.ModeCharDevice == 0 {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := scanner.Text()
			if err := s.ExecNonInteractive(line, os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			}
		}
		return
	}

	if err := s.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "shell error: %v\n", err)
		os.Exit(1)
	}
}

func dimText(s string) string {
	return "\033[90m" + s + "\033[0m"
}

func runSetup() {
	args := os.Args[2:]
	var app, customer, sampleFile string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--app":
			if i+1 < len(args) {
				app = args[i+1]
				i++
			}
		case "--customer":
			if i+1 < len(args) {
				customer = args[i+1]
				i++
			}
		case "--sample":
			if i+1 < len(args) {
				sampleFile = args[i+1]
				i++
			}
		}
	}

	if app == "" || sampleFile == "" {
		fmt.Fprintln(os.Stderr, "usage: weft setup --app NAME --sample FILE [--customer CUST]")
		os.Exit(1)
	}
	if customer == "" {
		customer = "default"
	}

	_, err := shell.RunSetup(app, customer, sampleFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup failed: %v\n", err)
		os.Exit(1)
	}
}

func runIngest() {
	args := os.Args[2:]
	var schemaFile, logFile, dbPath string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--schema":
			if i+1 < len(args) {
				schemaFile = args[i+1]
				i++
			}
		case "--file":
			if i+1 < len(args) {
				logFile = args[i+1]
				i++
			}
		case "--db":
			if i+1 < len(args) {
				dbPath = args[i+1]
				i++
			}
		}
	}

	if schemaFile == "" || logFile == "" {
		fmt.Fprintln(os.Stderr, "usage: weft ingest --schema FILE --file LOGFILE [--db PATH]")
		os.Exit(1)
	}

	schema, err := ingest.LoadSchema(schemaFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load schema: %v\n", err)
		os.Exit(1)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read log file: %v\n", err)
		os.Exit(1)
	}
	lines := strings.Split(string(data), "\n")

	parser, err := ingest.NewParser(schema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create parser: %v\n", err)
		os.Exit(1)
	}

	now := time.Now().UTC()
	rows, err := parser.ParseLines(lines, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse lines: %v\n", err)
		os.Exit(1)
	}

	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "no rows parsed from log file")
		os.Exit(1)
	}

	minTs, maxTs := rows[0].Ts, rows[0].Ts
	for _, r := range rows[1:] {
		if r.Ts.Before(minTs) {
			minTs = r.Ts
		}
		if r.Ts.After(maxTs) {
			maxTs = r.Ts
		}
	}

	if dbPath == "" {
		dbPath = fmt.Sprintf("%s.duckdb", schema.App)
	}
	ds, err := shard.NewDuckDBShard("ingest", dbPath, minTs, maxTs.Add(time.Second))
	if err != nil {
		fmt.Fprintf(os.Stderr, "open shard: %v\n", err)
		os.Exit(1)
	}
	defer ds.Close()

	if err := ds.Ingest(rows); err != nil {
		fmt.Fprintf(os.Stderr, "ingest: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Ingested %d rows into %s (time range: %s to %s)\n",
		len(rows), dbPath,
		minTs.Format("2006-01-02 15:04:05"),
		maxTs.Format("2006-01-02 15:04:05"))
}
