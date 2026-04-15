package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
		case "-c":
			if len(os.Args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: weft -c 'pipeline'")
				os.Exit(1)
			}
			runOneShot(strings.Join(os.Args[2:], " "))
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
			r, err := coord.Query(pipeline, tr)
			if err != nil {
				return nil, err
			}
			return &shell.QueryResult{Rows: r.Rows, Text: r.Text}, nil
		},
		Start: coordinator.DevT0,
		End:   coordinator.DevT2,
		Apps:  apps,
		// Dev shell uses in-memory FS (no gRPC coordinator to proxy through).
		FS: shell.NewDevFS(apps),
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

func connectRemote(cfg *shell.Config) (*shell.Shell, func(), error) {
	endpoint := cfg.Endpoint

	// HTTP mode: endpoint is a URL (https://api.w3ft.net).
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		if cfg.Token == "" {
			return nil, nil, fmt.Errorf("WEFT_TOKEN required for HTTP mode — get one from the web dashboard")
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
			FS:     shell.NewHTTPRemoteFS(endpoint, cfg.Token, apps),
		}
		return s, func() {}, nil
	}

	// gRPC mode: endpoint is host:port.
	remote, err := shell.NewRemoteCoordinator(endpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to %s: %w", endpoint, err)
	}

	apps, err := remote.ListApps("") // gRPC mode: customer filtering happens at query time
	if err != nil {
		remote.Close()
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
		FS:     shell.NewRemoteFS(remote.Client(), cfg.Customer, apps),
	}
	return s, func() { remote.Close() }, nil
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
		fmt.Fprintln(os.Stderr, "usage: weft source {add|list|remove}")
		os.Exit(1)
	}

	cfg, err := shell.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no .weft.yaml: %v\n", err)
		os.Exit(1)
	}
	remote, err := shell.NewRemoteCoordinator(cfg.Endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(1)
	}
	defer remote.Close()

	switch os.Args[2] {
	case "add":
		runSourceAdd(remote, cfg)
	case "list":
		runSourceList(remote)
	case "remove":
		runSourceRemove(remote)
	default:
		fmt.Fprintf(os.Stderr, "unknown source command: %s\n", os.Args[2])
		os.Exit(1)
	}
}

func runSourceAdd(remote *shell.RemoteCoordinator, cfg *shell.Config) {
	args := os.Args[3:]
	var app, logGroup, schemaFile, region, pollInterval, awsAccountID, roleARN, externalID string

	for i := 0; i < len(args); i++ {
		switch args[i] {
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

	if app == "" || logGroup == "" || schemaFile == "" || roleARN == "" {
		fmt.Fprintln(os.Stderr, `usage: weft source add --app NAME --log-group GROUP --schema FILE --role-arn ARN
    [--account AWS_ACCOUNT_ID] [--region REGION] [--interval DURATION] [--external-id ID]`)
		os.Exit(1)
	}
	if region == "" {
		region = "us-east-2"
	}
	if pollInterval == "" {
		pollInterval = "30s"
	}

	schemaData, err := os.ReadFile(schemaFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read schema: %v\n", err)
		os.Exit(1)
	}

	customer := cfg.Customer
	if customer == "" {
		customer = "default"
	}

	if err := remote.AddSource(app, customer, logGroup, region, string(schemaData), pollInterval, awsAccountID, roleARN, externalID); err != nil {
		fmt.Fprintf(os.Stderr, "add source: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Added source: %s → %s (account %s, poll every %s)\n", app, logGroup, awsAccountID, pollInterval)
}

func runSourceList(remote *shell.RemoteCoordinator) {
	sources, err := remote.ListSources()
	if err != nil {
		fmt.Fprintf(os.Stderr, "list sources: %v\n", err)
		os.Exit(1)
	}
	if len(sources) == 0 {
		fmt.Println("No sources configured.")
		return
	}
	fmt.Printf("%-20s %-10s %-35s %-12s %s\n", "APP", "CUSTOMER", "LOG GROUP", "REGION", "LAST POLL")
	for _, s := range sources {
		lastPoll := s.LastPollAt
		if lastPoll == "" {
			lastPoll = "(never)"
		}
		fmt.Printf("%-20s %-10s %-35s %-12s %s\n", s.App, s.Customer, s.LogGroup, s.Region, lastPoll)
	}
}

func runSourceRemove(remote *shell.RemoteCoordinator) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: weft source remove --app NAME")
		os.Exit(1)
	}
	args := os.Args[3:]
	var app string
	for i := 0; i < len(args); i++ {
		if args[i] == "--app" && i+1 < len(args) {
			app = args[i+1]
			i++
		}
	}
	if app == "" {
		fmt.Fprintln(os.Stderr, "usage: weft source remove --app NAME")
		os.Exit(1)
	}

	if err := remote.RemoveSource(app); err != nil {
		fmt.Fprintf(os.Stderr, "remove source: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Removed source: %s\n", app)
}

// tokenClient is the common interface for token operations across HTTP and gRPC.
type tokenClient interface {
	CreateToken(customer, label, app string) (string, error)
	ListTokens(customer string) ([]shell.TokenInfo, error)
	RevokeToken(token string) error
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

	var client tokenClient
	var cleanup func()

	if strings.HasPrefix(cfg.Endpoint, "http://") || strings.HasPrefix(cfg.Endpoint, "https://") {
		client = shell.NewHTTPCoordinator(cfg.Endpoint, cfg.Token)
		cleanup = func() {}
	} else {
		remote, err := shell.NewRemoteCoordinator(cfg.Endpoint)
		if err != nil {
			fmt.Fprintf(os.Stderr, "connect: %v\n", err)
			os.Exit(1)
		}
		client = remote
		cleanup = func() { remote.Close() }
	}
	defer cleanup()

	switch os.Args[2] {
	case "create":
		runTokenCreate(client, cfg)
	case "list":
		runTokenList(client, cfg)
	case "revoke":
		runTokenRevoke(client)
	default:
		fmt.Fprintf(os.Stderr, "unknown token command: %s\n", os.Args[2])
		os.Exit(1)
	}
}

func runTokenCreate(client tokenClient, cfg *shell.Config) {
	args := os.Args[3:]
	var label, namespace string

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
		}
	}

	if namespace == "" {
		fmt.Fprintln(os.Stderr, "usage: weft token create --namespace NAME --label LABEL")
		os.Exit(1)
	}

	token, err := client.CreateToken("", label, namespace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create token: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Token created: %s\n", token)
	if label != "" {
		fmt.Printf("Label: %s\n", label)
	}
	fmt.Println("Store this token in your weftd config. It cannot be retrieved again.")
}

func runTokenList(remote tokenClient, cfg *shell.Config) {
	customer := cfg.Customer
	args := os.Args[3:]
	for i := 0; i < len(args); i++ {
		if args[i] == "--customer" && i+1 < len(args) {
			customer = args[i+1]
			i++
		}
	}

	tokens, err := remote.ListTokens(customer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list tokens: %v\n", err)
		os.Exit(1)
	}
	if len(tokens) == 0 {
		fmt.Println("No tokens configured.")
		return
	}
	fmt.Printf("%-40s %-15s %-25s %-10s %s\n", "TOKEN", "APP", "LABEL", "STATUS", "CREATED")
	for _, t := range tokens {
		status := "active"
		if t.Revoked {
			status = "revoked"
		}
		display := t.Token[:12] + "..."
		fmt.Printf("%-40s %-15s %-25s %-10s %s\n", display, t.App, t.Label, status, t.CreatedAt)
	}
}

func runTokenRevoke(remote tokenClient) {
	args := os.Args[3:]
	var token string
	for i := 0; i < len(args); i++ {
		if args[i] == "--token" && i+1 < len(args) {
			token = args[i+1]
			i++
		}
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "usage: weft token revoke --token TOKEN")
		os.Exit(1)
	}
	if err := remote.RevokeToken(token); err != nil {
		fmt.Fprintf(os.Stderr, "revoke token: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Revoked token: %s\n", token[:12]+"...")
}
