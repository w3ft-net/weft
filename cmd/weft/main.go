package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
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
		case "sh":
			// weft sh [-c 'pipeline']
			if len(os.Args) > 2 && os.Args[2] == "-c" {
				if len(os.Args) < 4 {
					fmt.Fprintln(os.Stderr, "usage: weft sh -c 'pipeline'")
					os.Exit(1)
				}
				runOneShot(strings.Join(os.Args[3:], " "))
				return
			}
			runShellCmd()
			return
		case "-c":
			// Legacy shorthand: weft -c 'pipeline'
			if len(os.Args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: weft -c 'pipeline'")
				os.Exit(1)
			}
			runOneShot(strings.Join(os.Args[2:], " "))
			return
		case "whoami":
			runWhoami()
			return
		case "login":
			runLogin()
			return
		case "namespace", "ns":
			runNamespace()
			return
		case "setup":
			runSetup()
			return
		case "ingest":
			runIngest()
			return
		case "source":
			runSource()
			return
		case "token":
			runToken()
			return
		}
	}

	printUsage()
}

func printUsage() {
	fmt.Println("weft — log analysis for operators")
	fmt.Println()
	fmt.Println("Usage: weft <command> [options]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  sh              Start the interactive shell")
	fmt.Println("  sh -c 'CMD'     Run a single pipeline and exit")
	fmt.Println("  whoami          Show current user, team, and plan")
	fmt.Println("  login           Authenticate via browser device flow")
	fmt.Println("  namespace       Manage namespaces (list, create)")
	fmt.Println("  token           Manage agent tokens (create, list, revoke)")
	fmt.Println("  source          Manage log sources (add, list, remove, pending, configure)")
	fmt.Println("  setup           Interactive schema setup from a sample file")
	fmt.Println("  ingest          Ingest a log file into a local DuckDB shard")
	fmt.Println()
	fmt.Println("Run 'weft <command> --help' for details on a specific command.")
}

func runShellCmd() {
	// Load config.
	cfg, err := shell.LoadConfig()
	if err != nil {
		// Fall back to dev mode with fixtures.
		fmt.Fprintln(os.Stderr, dimText("no .weft.yaml found, using dev fixtures"))
		runDevShell()
		return
	}

	s, cleanup, err := connectRemote(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	runShell(s)
}

func runWhoami() {
	cfg, err := shell.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Not logged in. Run 'weft login' to authenticate.")
		os.Exit(1)
	}

	if cfg.Token == "" {
		fmt.Fprintln(os.Stderr, "No token in ~/.weft.yaml. Run 'weft login' to authenticate.")
		os.Exit(1)
	}

	// Decode JWT payload without signature verification (we don't have the secret).
	parts := strings.Split(cfg.Token, ".")
	if len(parts) != 3 {
		fmt.Fprintln(os.Stderr, "Invalid token format.")
		os.Exit(1)
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to decode token.")
		os.Exit(1)
	}

	var claims struct {
		Email    string `json:"email"`
		Customer string `json:"customer"`
		Tier     string `json:"tier"`
		IssuedAt int64  `json:"iat"`
		Expires  int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		fmt.Fprintln(os.Stderr, "Failed to parse token claims.")
		os.Exit(1)
	}

	expires := time.Unix(claims.Expires, 0)
	expired := time.Now().After(expires)

	fmt.Printf("Email:    %s\n", claims.Email)
	fmt.Printf("Team:     %s\n", claims.Customer)
	fmt.Printf("Plan:     %s\n", claims.Tier)
	fmt.Printf("Endpoint: %s\n", cfg.Endpoint)
	if expired {
		fmt.Printf("Token:    expired (%s) — run 'weft login' to re-authenticate\n", expires.Format("2006-01-02"))
	} else {
		fmt.Printf("Token:    valid until %s\n", expires.Format("2006-01-02"))
	}
}

func runDevShell() {
	coord, cleanup, err := coordinator.NewDevCoordinator()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start: %v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	apps := coordinator.DevApps()
	s := &shell.Shell{
		Query: func(pipeline string, start, end time.Time) (*shell.QueryResult, error) {
			tr := coordinator.TimeRange{Start: start, End: end}
			r, err := coord.Query(pipeline, tr, "kevlar", "")
			if err != nil {
				return nil, err
			}
			return &shell.QueryResult{Rows: r.Rows, Text: r.Text}, nil
		},
		Start: coordinator.DevT0,
		End:   coordinator.DevT2,
		Apps:  apps,
		// Dev shell uses in-memory FS (no gRPC coordinator to proxy through).
		FS: shell.NewRemoteFS(shell.NewMemBackend(), apps),
	}

	runShell(s)
}

func runShell(s *shell.Shell) {
	// Set user for the prompt if not already set.
	if s.User == "" {
		if u, err := user.Current(); err == nil {
			s.User = u.Username
		}
	}

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

func runOneShot(pipeline string) {
	cfg, err := shell.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no .weft.yaml: %v\n", err)
		os.Exit(1)
	}

	s, cleanup, err := connectRemote(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	if err := s.ExecNonInteractive(pipeline, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func runLogin() {
	baseURL := os.Getenv("WEFT_ENDPOINT")
	if baseURL == "" {
		baseURL = "https://api.w3ft.net"
	}

	// Request a device code.
	resp, err := http.Post(baseURL+"/api/auth/device", "application/json", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "request device code: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "request device code: HTTP %d: %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}

	var device struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		ExpiresIn  int    `json:"expires_in"`
	}
	json.Unmarshal(body, &device)

	// Build the browser URL.
	webBase := strings.Replace(baseURL, "api.", "", 1) // https://api.w3ft.net → https://w3ft.net
	authURL := fmt.Sprintf("%s/device?code=%s", webBase, device.UserCode)

	fmt.Printf("Open this URL to authorize the CLI:\n\n  %s\n\n", authURL)
	fmt.Printf("Verify code: %s\n\n", device.UserCode)

	// Try to open the browser.
	if err := exec.Command("open", authURL).Start(); err != nil {
		// Try xdg-open on Linux.
		exec.Command("xdg-open", authURL).Start()
	}

	fmt.Printf("Waiting for authorization...")

	// Poll until approved or expired.
	client := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)

	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		fmt.Print(".")

		pollBody, _ := json.Marshal(map[string]string{"device_code": device.DeviceCode})
		pollResp, err := client.Post(baseURL+"/api/auth/device/poll", "application/json", bytes.NewReader(pollBody))
		if err != nil {
			continue
		}
		pollData, _ := io.ReadAll(pollResp.Body)
		pollResp.Body.Close()

		if pollResp.StatusCode == http.StatusGone {
			fmt.Fprintf(os.Stderr, "\nDevice code expired.\n")
			os.Exit(1)
		}

		var result struct {
			Status   string `json:"status"`
			Token    string `json:"token"`
			Email    string `json:"email"`
			Customer string `json:"customer"`
		}
		json.Unmarshal(pollData, &result)

		if result.Status == "approved" {
			fmt.Printf("\n\nLogged in as %s (%s)\n", result.Email, result.Customer)

			// Write config file.
			home, _ := os.UserHomeDir()
			configPath := filepath.Join(home, ".weft.yaml")
			config := fmt.Sprintf("endpoint: %s\ntoken: %s\n", baseURL, result.Token)
			if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write %s: %v\n", configPath, err)
				fmt.Printf("\nAdd this to your ~/.weft.yaml manually:\n\n  endpoint: %s\n  token: %s\n", baseURL, result.Token)
			} else {
				fmt.Printf("Config written to %s\n", configPath)
			}
			return
		}
	}

	fmt.Fprintf(os.Stderr, "\nTimed out waiting for authorization.\n")
	os.Exit(1)
}

func runNamespace() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: weft namespace {list|create}")
		os.Exit(1)
	}

	cfg, err := shell.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no .weft.yaml: %v\n", err)
		os.Exit(1)
	}

	if !strings.HasPrefix(cfg.Endpoint, "http") {
		fmt.Fprintln(os.Stderr, "namespace commands require HTTP endpoint")
		os.Exit(1)
	}

	remote := shell.NewHTTPCoordinator(cfg.Endpoint, cfg.Token)

	switch os.Args[2] {
	case "list":
		nss, err := remote.ListNamespaces()
		if err != nil {
			fmt.Fprintf(os.Stderr, "list namespaces: %v\n", err)
			os.Exit(1)
		}
		if len(nss) == 0 {
			fmt.Println("No namespaces. Create one with: weft namespace create --name <name>")
			return
		}
		for _, ns := range nss {
			fmt.Println(ns.Name)
		}

	case "create":
		args := os.Args[3:]
		var name string
		for i := 0; i < len(args); i++ {
			if args[i] == "--name" && i+1 < len(args) {
				name = args[i+1]
				i++
			}
		}
		if name == "" && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			name = args[0]
		}
		if name == "" {
			fmt.Fprintln(os.Stderr, "usage: weft namespace create --name <name>")
			os.Exit(1)
		}
		ns, err := remote.CreateNamespace(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create namespace: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Created namespace: %s\n", ns.Name)

	default:
		fmt.Fprintf(os.Stderr, "unknown namespace command: %s\n", os.Args[2])
		os.Exit(1)
	}
}

// connectRemote builds a shell backed by the weft api server over HTTP.
// The CLI only speaks HTTP now — direct-to-coordinator gRPC was retired
// because it doubled the public auth surface (the api server already
// gates JWT, email verification, rate limits, CORS, etc). Operators who
// need a gRPC tap can still point pb.WeftCoordinatorClient at :9020
// from the internal network; it's not the CLI's job to expose that.
func connectRemote(cfg *shell.Config) (*shell.Shell, func(), error) {
	endpoint := cfg.Endpoint
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		return nil, nil, fmt.Errorf("endpoint %q is not an api URL — re-run `weft login` "+
			"or edit ~/.weft.yaml to point at http(s)://<api-host>", endpoint)
	}
	if cfg.Token == "" {
		return nil, nil, fmt.Errorf("WEFT_TOKEN required — run `weft login` to get one")
	}

	remote := shell.NewHTTPCoordinator(endpoint, cfg.Token)
	apps, err := remote.ListApps()
	if err != nil {
		return nil, nil, fmt.Errorf("list apps: %w", err)
	}

	s := &shell.Shell{
		Query: func(pipeline string, start, end time.Time) (*shell.QueryResult, error) {
			r, err := remote.Query(pipeline, start, end)
			if err != nil {
				return nil, err
			}
			return &shell.QueryResult{Rows: r.Rows, Text: r.Text}, nil
		},
		Follow: remote.Follow,
		Start:  time.Now().Add(-24 * time.Hour),
		End:    time.Now(),
		Apps:   apps,
		FS:     shell.NewRemoteFS(shell.NewHTTPBackend(endpoint, cfg.Token), apps),
	}
	return s, func() {}, nil
}

func runSourcePending(cfg *shell.Config) {
	endpoint := cfg.Endpoint
	if !strings.HasPrefix(endpoint, "http") {
		fmt.Fprintln(os.Stderr, "source pending requires HTTP endpoint")
		os.Exit(1)
	}
	remote := shell.NewHTTPCoordinator(endpoint, cfg.Token)

	var namespace string
	args := os.Args[3:]
	for i := 0; i < len(args); i++ {
		if args[i] == "--namespace" || args[i] == "-n" {
			if i+1 < len(args) {
				namespace = args[i+1]
			}
		}
	}

	sources, err := remote.ListPendingSources(namespace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list pending: %v\n", err)
		os.Exit(1)
	}

	if len(sources) == 0 {
		fmt.Println("No pending sources.")
		return
	}

	fmt.Printf("%-20s %-15s %-20s %-15s %s\n", "APP", "NAMESPACE", "INSTANCE", "DEPLOYMENT", "AGE")
	for _, s := range sources {
		fmt.Printf("%-20s %-15s %-20s %-15s %s\n", s.App, s.Namespace, s.Instance, s.Deployment, s.CreatedAt)
	}
}

func runSourceConfigure(cfg *shell.Config) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: weft source configure <namespace>/<app>")
		os.Exit(1)
	}
	endpoint := cfg.Endpoint
	if !strings.HasPrefix(endpoint, "http") {
		fmt.Fprintln(os.Stderr, "source configure requires HTTP endpoint")
		os.Exit(1)
	}
	remote := shell.NewHTTPCoordinator(endpoint, cfg.Token)

	target := os.Args[3]
	parts := strings.SplitN(target, "/", 2)
	if len(parts) != 2 {
		fmt.Fprintln(os.Stderr, "usage: weft source configure <namespace>/<app>")
		os.Exit(1)
	}
	namespace, app := parts[0], parts[1]

	// Fetch the pending source to get sample + detection.
	sources, err := remote.ListPendingSources(namespace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list pending: %v\n", err)
		os.Exit(1)
	}

	var found *shell.PendingSource
	for i := range sources {
		if sources[i].Namespace == namespace && sources[i].App == app {
			found = &sources[i]
			break
		}
	}
	if found == nil {
		fmt.Fprintf(os.Stderr, "no pending source: %s/%s\n", namespace, app)
		os.Exit(1)
	}

	// Parse sample lines from JSON.
	var lines []string
	if err := json.Unmarshal(found.Sample, &lines); err != nil {
		fmt.Fprintf(os.Stderr, "parse sample: %v\n", err)
		os.Exit(1)
	}

	// Run the interactive setup wizard with the remote sample.
	schema, err := shell.RunSetupFromLines(namespace+"/"+app, lines)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup: %v\n", err)
		os.Exit(1)
	}

	// Send the configuration to the coordinator.
	config := map[string]any{
		"format":           schema.Format,
		"timestamp_field":  schema.TimestampField,
		"timestamp_format": schema.TimestampFormat,
		"level_field":      schema.LevelField,
		"level_pattern":    schema.LevelPattern,
		"expand_keys":      schema.ExpandKeys,
	}
	if err := remote.ConfigureSource(namespace, app, config); err != nil {
		fmt.Fprintf(os.Stderr, "configure: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Source %s/%s configured.\n", namespace, app)
}

func dimText(s string) string {
	return "\033[90m" + s + "\033[0m"
}

func runSetup() {
	args := os.Args[2:]
	var app, sampleFile string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--app":
			if i+1 < len(args) {
				app = args[i+1]
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
		fmt.Fprintln(os.Stderr, "usage: weft setup --app NAME --sample FILE")
		os.Exit(1)
	}

	_, err := shell.RunSetup(app, sampleFile)
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

	minNs, maxNs := rows[0].TsUnixNs, rows[0].TsUnixNs
	for _, r := range rows[1:] {
		if r.TsUnixNs < minNs {
			minNs = r.TsUnixNs
		}
		if r.TsUnixNs > maxNs {
			maxNs = r.TsUnixNs
		}
	}
	minTs := time.Unix(0, minNs)
	maxTs := time.Unix(0, maxNs)

	if dbPath == "" {
		dbPath = fmt.Sprintf("%s.duckdb", schema.App)
	}
	ds, err := shard.NewDuckDBShard("ingest", dbPath)
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

func runSource() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: weft source {add|list|remove|pending|configure}")
		os.Exit(1)
	}

	cfg, err := shell.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no .weft.yaml: %v\n", err)
		os.Exit(1)
	}
	client, err := httpClient(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	switch os.Args[2] {
	case "add":
		runSourceAdd(client)
	case "list":
		runSourceList(client)
	case "remove":
		runSourceRemove(client)
	case "pending":
		runSourcePending(cfg)
	case "configure":
		runSourceConfigure(cfg)
	default:
		fmt.Fprintf(os.Stderr, "unknown source command: %s\n", os.Args[2])
		os.Exit(1)
	}
}

// httpClient returns an HTTPCoordinator built from cfg, with a uniform
// error message if the endpoint isn't an api URL or WEFT_TOKEN is unset.
// Used by every subcommand that needs to hit the api.
func httpClient(cfg *shell.Config) (*shell.HTTPCoordinator, error) {
	if !strings.HasPrefix(cfg.Endpoint, "http://") && !strings.HasPrefix(cfg.Endpoint, "https://") {
		return nil, fmt.Errorf("endpoint %q is not an api URL — re-run `weft login`", cfg.Endpoint)
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("WEFT_TOKEN required — run `weft login`")
	}
	return shell.NewHTTPCoordinator(cfg.Endpoint, cfg.Token), nil
}

func runSourceAdd(client *shell.HTTPCoordinator) {
	args := os.Args[3:]
	var namespace, app, logGroup, schemaFile, region, pollInterval, awsAccountID, roleARN, externalID string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--namespace":
			if i+1 < len(args) {
				namespace = args[i+1]
				i++
			}
		case "--app":
			if i+1 < len(args) {
				app = args[i+1]
				i++
			}
		case "--log-group":
			if i+1 < len(args) {
				logGroup = args[i+1]
				i++
			}
		case "--schema":
			if i+1 < len(args) {
				schemaFile = args[i+1]
				i++
			}
		case "--region":
			if i+1 < len(args) {
				region = args[i+1]
				i++
			}
		case "--interval":
			if i+1 < len(args) {
				pollInterval = args[i+1]
				i++
			}
		case "--account":
			if i+1 < len(args) {
				awsAccountID = args[i+1]
				i++
			}
		case "--role-arn":
			if i+1 < len(args) {
				roleARN = args[i+1]
				i++
			}
		case "--external-id":
			if i+1 < len(args) {
				externalID = args[i+1]
				i++
			}
		}
	}

	if namespace == "" || app == "" || logGroup == "" || schemaFile == "" || roleARN == "" {
		fmt.Fprintln(os.Stderr, `usage: weft source add --namespace NS --app NAME --log-group GROUP --schema FILE --role-arn ARN
    [--account AWS_ACCOUNT_ID] [--region REGION] [--interval DURATION] [--external-id ID]`)
		os.Exit(1)
	}
	if strings.Contains(app, "/") {
		fmt.Fprintln(os.Stderr, "--app should be the sub-app name only; pass the namespace via --namespace")
		os.Exit(1)
	}
	if region == "" {
		region = "us-east-2"
	}
	if pollInterval == "" {
		pollInterval = "30s"
	}

	schemaYAML, err := os.ReadFile(schemaFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read schema: %v\n", err)
		os.Exit(1)
	}

	// The api server builds the Schema YAML from the individual fields in
	// AddCloudWatchSourceRequest rather than accepting a YAML blob. Parse
	// the user-supplied file locally and pass the fields through.
	parsedSchema, err := ingest.LoadSchemaFromBytes(schemaYAML)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse schema %s: %v\n", schemaFile, err)
		os.Exit(1)
	}

	if err := client.AddCloudWatchSource(shell.AddCloudWatchSourceRequest{
		Namespace:       namespace,
		App:             app,
		AWSAccountID:    awsAccountID,
		RoleARN:         roleARN,
		ExternalID:      externalID,
		LogGroup:        logGroup,
		Region:          region,
		PollInterval:    pollInterval,
		Format:          parsedSchema.Format,
		TimestampField:  parsedSchema.TimestampField,
		TimestampFormat: parsedSchema.TimestampFormat,
		LevelField:      parsedSchema.LevelField,
		LevelPattern:    parsedSchema.LevelPattern,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "add source: %v\n", err)
		os.Exit(1)
	}
	fullApp := namespace + "/" + app
	fmt.Printf("Added source: %s → %s (account %s, poll every %s)\n", fullApp, logGroup, awsAccountID, pollInterval)
}

func runSourceList(client *shell.HTTPCoordinator) {
	sources, err := client.ListSources()
	if err != nil {
		fmt.Fprintf(os.Stderr, "list sources: %v\n", err)
		os.Exit(1)
	}
	if len(sources) == 0 {
		fmt.Println("No sources configured.")
		return
	}
	fmt.Printf("%-15s %-20s %-10s %-35s %-12s %s\n", "NAMESPACE", "APP", "SOURCE", "LOG GROUP", "REGION", "LAST POLL")
	for _, s := range sources {
		lastPoll := s.LastPollAt
		if lastPoll == "" {
			lastPoll = "(never)"
		}
		fmt.Printf("%-15s %-20s %-10s %-35s %-12s %s\n", s.Namespace, s.App, s.SourceType, s.LogGroup, s.Region, lastPoll)
	}
}

func runSourceRemove(client *shell.HTTPCoordinator) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: weft source remove --namespace NS --app NAME")
		os.Exit(1)
	}
	args := os.Args[3:]
	var namespace, app string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--namespace":
			if i+1 < len(args) {
				namespace = args[i+1]
				i++
			}
		case "--app":
			if i+1 < len(args) {
				app = args[i+1]
				i++
			}
		}
	}
	if namespace == "" || app == "" {
		fmt.Fprintln(os.Stderr, "usage: weft source remove --namespace NS --app NAME")
		os.Exit(1)
	}
	if err := client.RemoveSource(namespace, app); err != nil {
		fmt.Fprintf(os.Stderr, "remove source: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Removed source: %s/%s\n", namespace, app)
}

func runToken() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: weft token {create|list|revoke}")
		os.Exit(1)
	}

	cfg, err := shell.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no .weft.yaml: %v\n", err)
		os.Exit(1)
	}
	client, err := httpClient(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	switch os.Args[2] {
	case "create":
		runTokenCreate(client)
	case "list":
		runTokenList(client)
	case "revoke":
		runTokenRevoke(client)
	default:
		fmt.Fprintf(os.Stderr, "unknown token command: %s\n", os.Args[2])
		os.Exit(1)
	}
}

func runTokenCreate(client *shell.HTTPCoordinator) {
	args := os.Args[3:]
	var label, namespace, env string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--label":
			if i+1 < len(args) {
				label = args[i+1]
				i++
			}
		case "--namespace", "--ns":
			if i+1 < len(args) {
				namespace = args[i+1]
				i++
			}
		case "--env":
			if i+1 < len(args) {
				env = args[i+1]
				i++
			}
		}
	}

	if namespace == "" {
		fmt.Fprintln(os.Stderr, "usage: weft token create --namespace NAME [--label LABEL] [--env ENV]")
		os.Exit(1)
	}

	token, err := client.CreateToken("", label, namespace, env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create token: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Token created: %s\n", token)
	if label != "" {
		fmt.Printf("Label: %s\n", label)
	}
	if env != "" {
		fmt.Printf("Env:   %s (stamped on every row from this token)\n", env)
	}
	fmt.Println("Store this token in your weftd config. It cannot be retrieved again.")
}

func runTokenList(client *shell.HTTPCoordinator) {
	tokens, err := client.ListTokens("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "list tokens: %v\n", err)
		os.Exit(1)
	}
	if len(tokens) == 0 {
		fmt.Println("No tokens configured.")
		return
	}
	// Raw token isn't available after creation — show ID for revoke and
	// Prefix so humans can distinguish tokens at a glance.
	fmt.Printf("%-6s %-14s %-15s %-25s %-10s %s\n", "ID", "PREFIX", "NAMESPACE", "LABEL", "STATUS", "CREATED")
	for _, t := range tokens {
		fmt.Printf("%-6d %-14s %-15s %-25s %-10s %s\n", t.ID, t.Prefix+"…", t.Namespace, t.Label, t.Status, t.CreatedAt)
	}
}

func runTokenRevoke(client *shell.HTTPCoordinator) {
	args := os.Args[3:]
	var idStr string
	for i := 0; i < len(args); i++ {
		if args[i] == "--id" && i+1 < len(args) {
			idStr = args[i+1]
			i++
		}
	}
	if idStr == "" {
		fmt.Fprintln(os.Stderr, "usage: weft token revoke --id ID   (see `weft token list`)")
		os.Exit(1)
	}
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid id: %v\n", err)
		os.Exit(1)
	}
	if err := client.RevokeToken(uint(id)); err != nil {
		fmt.Fprintf(os.Stderr, "revoke token: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Revoked token id %d\n", id)
}
