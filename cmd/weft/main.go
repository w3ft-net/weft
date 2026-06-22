package main

import (
	"bufio"
	"bytes"
	"context"
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

	"github.com/graybeardcorp/weft/cmd/weft/runner"
	"github.com/graybeardcorp/weft/cmd/weft/setup"
	"github.com/graybeardcorp/weft/internal/timerange"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shell"
)

// orgOverride is the active org chosen via --org / WEFT_ORG, applied
// on top of whatever the yaml config says. Set once at startup by
// extractTopLevelFlags so any LoadConfig() consumer picks it up
// without threading the flag explicitly.
var orgOverride string

// extractTopLevelFlags pulls global flags (currently just --org) out of
// os.Args before subcommand dispatch. Supports both --org=NAME and
// --org NAME forms, and the short -o variant. Removes them from
// os.Args so subcommands' own arg parsing isn't tripped up.
func extractTopLevelFlags() {
	out := []string{os.Args[0]}
	for i := 1; i < len(os.Args); i++ {
		a := os.Args[i]
		switch {
		case a == "--org" || a == "-o":
			if i+1 < len(os.Args) {
				orgOverride = os.Args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--org="):
			orgOverride = strings.TrimPrefix(a, "--org=")
		case strings.HasPrefix(a, "-o="):
			orgOverride = strings.TrimPrefix(a, "-o=")
		default:
			out = append(out, a)
		}
	}
	os.Args = out
}

func main() {
	extractTopLevelFlags()
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "sh":
			// weft sh [--range <spec>] [-c 'pipeline']
			args := os.Args[2:]
			var rangeSpec string
			for i := 0; i < len(args); i++ {
				if args[i] == "--range" && i+1 < len(args) {
					rangeSpec = args[i+1]
					args = append(args[:i], args[i+2:]...)
					break
				}
				if strings.HasPrefix(args[i], "--range=") {
					rangeSpec = strings.TrimPrefix(args[i], "--range=")
					args = append(args[:i], args[i+1:]...)
					break
				}
			}
			if len(args) > 0 && args[0] == "-c" {
				if len(args) < 2 {
					fmt.Fprintln(os.Stderr, "usage: weft sh [--range <start>..<end>] -c 'pipeline'")
					os.Exit(1)
				}
				runOneShot(strings.Join(args[1:], " "), rangeSpec)
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
			runOneShot(strings.Join(os.Args[2:], " "), "")
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
		case "org":
			runOrg()
			return
		case "source":
			runSource()
			return
		case "token":
			runToken()
			return
		case "integration", "integrations":
			runIntegration()
			return
		case "mcp":
			runMCP()
			return
		case "propose":
			runPropose()
			return
		case "update":
			runUpdate()
			return
		case "--version", "-v", "version":
			fmt.Println(ClientID())
			return
		}
	}

	printUsage()
}

// effectiveOrg returns the active org for the current invocation:
// --org flag (or WEFT_ORG env, applied during LoadConfig) wins over
// the yaml-stored org.
func effectiveOrg(cfg *shell.Config) string {
	if orgOverride != "" {
		return orgOverride
	}
	return cfg.Org
}

func printUsage() {
	fmt.Println("weft — log analysis for your Unix heart")
	fmt.Println()
	fmt.Println("Usage: weft <command> [options]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  sh              Start the interactive shell")
	fmt.Println("  sh -c 'CMD'     Run a single pipeline and exit")
	fmt.Println("  whoami          Show current user, team, and plan")
	fmt.Println("  login           Authenticate via browser device flow")
	fmt.Println("  org             List or switch active org (e.g. weft org switch demo-source)")
	fmt.Println("  namespace       Manage namespaces (list, create)")
	fmt.Println("  token           Manage agent tokens (create, list, revoke)")
	fmt.Println("  source          Manage log sources (add, list, remove, pending, configure)")
	fmt.Println("  mcp             Start an MCP server on stdio for LLM clients")
	fmt.Println("  propose         Review or apply MCP-staged mutations (review|apply|clear)")
	fmt.Println("  update          Replace this binary with the latest released weft")
	fmt.Println()
	fmt.Println("Run 'weft <command> --help' for details on a specific command.")
}

func runShellCmd() {
	cfg, err := shell.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Not logged in. Run 'weft login' to authenticate.")
		os.Exit(1)
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

	// Token expiry comes from the JWT itself (no DB hit needed).
	claims, err := shell.DecodeClaims(cfg.Token)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	expires := time.Unix(claims.Expires, 0)
	expired := time.Now().After(expires)

	// Identity (email, team, plan) lives on the server now — the JWT
	// only carries the user ID. Fetch from /api/auth/me so display
	// reflects current DB state (including the active org from the
	// X-Active-Org header).
	req, _ := http.NewRequest("GET", cfg.Endpoint+"/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	if org := effectiveOrg(cfg); org != "" {
		req.Header.Set("X-Active-Org", org)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch identity: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "identity lookup failed (%d): %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}
	var me struct {
		Email    string `json:"email"`
		Customer string `json:"customer"`
		Tier     string `json:"tier"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		fmt.Fprintf(os.Stderr, "parse identity: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Email:    %s\n", me.Email)
	fmt.Printf("Team:     %s\n", me.Customer)
	fmt.Printf("Plan:     %s\n", me.Tier)
	fmt.Printf("Endpoint: %s\n", cfg.Endpoint)
	if expired {
		fmt.Printf("Token:    expired (%s) — run 'weft login' to re-authenticate\n", expires.Format("2006-01-02"))
	} else {
		fmt.Printf("Token:    valid until %s\n", expires.Format("2006-01-02"))
	}
}

// fetchPromptIdentity calls /api/auth/me to get the email + customer
// for prompt rendering. Returns ok=false on any failure; callers fall
// through to the OS user so a network blip never blocks the shell.
func fetchPromptIdentity(endpoint, token, org string) (email, customer string, ok bool) {
	req, err := http.NewRequest("GET", endpoint+"/api/auth/me", nil)
	if err != nil {
		return "", "", false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if org != "" {
		req.Header.Set("X-Active-Org", org)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", false
	}
	var me struct {
		Email    string `json:"email"`
		Customer string `json:"customer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		return "", "", false
	}
	return me.Email, me.Customer, true
}

func runShell(s *shell.Shell) {
	// Identity for the `alice@acme` prompt comes from the API now —
	// the JWT carries only the user ID. Fall through to OS user if
	// the lookup fails so a transient network error doesn't block
	// the shell from starting.
	if s.User == "" || s.Customer == "" {
		if cfg, err := shell.LoadConfig(); err == nil && cfg.Token != "" {
			if email, customer, ok := fetchPromptIdentity(cfg.Endpoint, cfg.Token, effectiveOrg(cfg)); ok {
				if s.User == "" {
					s.User = shell.EmailLocalPart(email)
				}
				if s.Customer == "" {
					s.Customer = customer
				}
			}
		}
	}
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

	if err := runner.Run(s); err != nil {
		fmt.Fprintf(os.Stderr, "shell error: %v\n", err)
		os.Exit(1)
	}
}

func runOneShot(pipeline, rangeSpec string) {
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

	if rangeSpec != "" {
		// Validate locally so a typo errors out before we round-trip to
		// the server. The server is the canonical resolver.
		if _, _, err := timerange.ParseRange(rangeSpec); err != nil {
			fmt.Fprintf(os.Stderr, "invalid --range: %v\n", err)
			os.Exit(1)
		}
		s.Range = rangeSpec
	}

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

	openBrowser(authURL)

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

	remote := shell.NewHTTPCoordinator(cfg.Endpoint, cfg.Token, effectiveOrg(cfg))

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

	remote := shell.NewHTTPCoordinator(endpoint, cfg.Token, effectiveOrg(cfg))
	remote.SetClientID(ClientID())
	apps, err := remote.ListApps()
	if err != nil {
		return nil, nil, fmt.Errorf("list apps: %w", err)
	}

	s := &shell.Shell{
		Query: func(pipeline, rangeSpec string, dirStack []string) (*shell.QueryResult, error) {
			r, err := remote.Query(pipeline, rangeSpec, dirStack)
			if err != nil {
				return nil, err
			}
			notices := make([]shell.QueryResultNotice, len(r.Notices))
			for i, n := range r.Notices {
				notices[i] = shell.QueryResultNotice{
					Key:   n.Key,
					Kind:  n.Kind,
					Title: n.Title,
					Body:  n.Body,
				}
			}
			rows := make([]shell.Row, len(r.Rows))
			for i, row := range r.Rows {
				rows[i] = shell.Row{Raw: row.Raw}
			}
			return &shell.QueryResult{Rows: rows, Text: r.Text, Notices: notices, FinalDirStack: r.FinalDirStack}, nil
		},
		Follow: func(ctx context.Context, pipeline, rangeSpec string, onRow func(shell.Row)) error {
			return remote.Follow(ctx, pipeline, rangeSpec, func(row *pb.ResultRow) {
				onRow(shell.Row{Raw: row.Raw})
			})
		},
		Range: "1d..",
		Apps:  apps,
		FS:    shell.NewProxyFS(endpoint, cfg.Token, effectiveOrg(cfg)),
	}
	return s, func() {}, nil
}

func runSourcePending(cfg *shell.Config) {
	endpoint := cfg.Endpoint
	if !strings.HasPrefix(endpoint, "http") {
		fmt.Fprintln(os.Stderr, "source pending requires HTTP endpoint")
		os.Exit(1)
	}
	remote := shell.NewHTTPCoordinator(endpoint, cfg.Token, effectiveOrg(cfg))

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

	fmt.Printf("%-20s %-15s %-20s %-15s %s\n", "SERVICE", "NAMESPACE", "INSTANCE", "DEPLOYMENT", "AGE")
	for _, s := range sources {
		fmt.Printf("%-20s %-15s %-20s %-15s %s\n", s.Service, s.Namespace, s.Instance, s.Deployment, s.CreatedAt)
	}
}

func runSourceConfigure(cfg *shell.Config) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: weft source configure <namespace>/<service>")
		os.Exit(1)
	}
	endpoint := cfg.Endpoint
	if !strings.HasPrefix(endpoint, "http") {
		fmt.Fprintln(os.Stderr, "source configure requires HTTP endpoint")
		os.Exit(1)
	}
	remote := shell.NewHTTPCoordinator(endpoint, cfg.Token, effectiveOrg(cfg))

	target := os.Args[3]
	parts := strings.SplitN(target, "/", 2)
	if len(parts) != 2 {
		fmt.Fprintln(os.Stderr, "usage: weft source configure <namespace>/<service>")
		os.Exit(1)
	}
	namespace, service := parts[0], parts[1]

	// A source is configurable when it has a sample to drive the wizard:
	// a pending first-time sample, or — for an already-configured source —
	// a fresh sample left by a completed resample. Both arrive on the
	// schema endpoint; the configure endpoint itself accepts either status.
	sch, err := remote.GetSourceSchema(namespace, service)
	if err != nil {
		fmt.Fprintf(os.Stderr, "schema: %v\n", err)
		os.Exit(1)
	}
	switch sch.Status {
	case "sampling":
		fmt.Fprintf(os.Stderr, "%s/%s hasn't collected a full sample yet (%d/%d lines). Try again once it has.\n",
			namespace, service, sch.SampleCollected, sch.SampleTarget)
		os.Exit(1)
	case "configured":
		if sch.ResampleRequested {
			fmt.Fprintf(os.Stderr, "resample of %s/%s in progress (%d/%d lines) — re-run configure once it completes.\n",
				namespace, service, sch.SampleCollected, sch.SampleTarget)
			os.Exit(1)
		}
		if len(sch.Sample) == 0 || string(sch.Sample) == "[]" {
			fmt.Fprintf(os.Stderr, "%s/%s is already configured. To change its schema, collect a fresh sample first:\n    weft source resample %s/%s\nthen re-run configure once it lands.\n",
				namespace, service, namespace, service)
			os.Exit(1)
		}
	case "pending":
		// First-time configure; the sample is on the response.
	default:
		fmt.Fprintf(os.Stderr, "%s/%s: unexpected status %q\n", namespace, service, sch.Status)
		os.Exit(1)
	}

	// Parse sample lines from JSON.
	var lines []string
	if err := json.Unmarshal(sch.Sample, &lines); err != nil {
		fmt.Fprintf(os.Stderr, "parse sample: %v\n", err)
		os.Exit(1)
	}
	if len(lines) == 0 {
		fmt.Fprintf(os.Stderr, "no sample lines available for %s/%s\n", namespace, service)
		os.Exit(1)
	}

	// Run the interactive setup wizard with the remote sample.
	schema, err := setup.RunSetupFromLines(namespace+"/"+service, lines)
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
	if err := remote.ConfigureSource(namespace, service, config); err != nil {
		fmt.Fprintf(os.Stderr, "configure: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Source %s/%s configured.\n", namespace, service)
}

// runSourceResample asks the agent for a fresh sample of an
// already-configured source. Ingestion keeps running on the current
// schema; the new sample shows up for review once the agent has
// accumulated a full one.
func runSourceResample(cfg *shell.Config) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: weft source resample <namespace>/<service>")
		os.Exit(1)
	}
	endpoint := cfg.Endpoint
	if !strings.HasPrefix(endpoint, "http") {
		fmt.Fprintln(os.Stderr, "source resample requires HTTP endpoint")
		os.Exit(1)
	}
	remote := shell.NewHTTPCoordinator(endpoint, cfg.Token, effectiveOrg(cfg))

	parts := strings.SplitN(os.Args[3], "/", 2)
	if len(parts) != 2 {
		fmt.Fprintln(os.Stderr, "usage: weft source resample <namespace>/<service>")
		os.Exit(1)
	}
	namespace, service := parts[0], parts[1]

	if err := remote.ResampleSource(namespace, service); err != nil {
		fmt.Fprintf(os.Stderr, "resample: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Resample requested for %s/%s. The agent will collect a fresh sample; ingestion continues on the current schema meanwhile.\n", namespace, service)
	fmt.Printf("Track progress with: weft source schema %s/%s\n", namespace, service)
}

// runSourceSchema prints the live schema for a configured source, or — if
// the source is still accumulating — how far along the sample is.
func runSourceSchema(cfg *shell.Config) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: weft source schema <namespace>/<service>")
		os.Exit(1)
	}
	endpoint := cfg.Endpoint
	if !strings.HasPrefix(endpoint, "http") {
		fmt.Fprintln(os.Stderr, "source schema requires HTTP endpoint")
		os.Exit(1)
	}
	remote := shell.NewHTTPCoordinator(endpoint, cfg.Token, effectiveOrg(cfg))

	parts := strings.SplitN(os.Args[3], "/", 2)
	if len(parts) != 2 {
		fmt.Fprintln(os.Stderr, "usage: weft source schema <namespace>/<service>")
		os.Exit(1)
	}
	namespace, service := parts[0], parts[1]

	sch, err := remote.GetSourceSchema(namespace, service)
	if err != nil {
		fmt.Fprintf(os.Stderr, "schema: %v\n", err)
		os.Exit(1)
	}

	switch sch.Status {
	case "configured":
		if sch.ResampleRequested {
			fmt.Fprintf(os.Stderr, "# resample in progress: %d/%d lines collected\n", sch.SampleCollected, sch.SampleTarget)
		}
		fmt.Print(sch.SchemaYAML)
		if !strings.HasSuffix(sch.SchemaYAML, "\n") {
			fmt.Println()
		}
	case "sampling":
		fmt.Printf("%s/%s hasn't emitted enough lines to sample yet (%d/%d).\n",
			namespace, service, sch.SampleCollected, sch.SampleTarget)
	default: // pending
		fmt.Printf("%s/%s has a sample awaiting configuration (%d/%d lines). Configure it with: weft source configure %s/%s\n",
			namespace, service, sch.SampleCollected, sch.SampleTarget, namespace, service)
	}
}

func dimText(s string) string {
	return "\033[90m" + s + "\033[0m"
}

func runSource() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: weft source {add|list|remove|pending|configure|resample|schema}")
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
	case "resample":
		runSourceResample(cfg)
	case "schema":
		runSourceSchema(cfg)
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
	return shell.NewHTTPCoordinator(cfg.Endpoint, cfg.Token, effectiveOrg(cfg)), nil
}

func runSourceAdd(client *shell.HTTPCoordinator) {
	args := os.Args[3:]
	var namespace, service, logGroup, region, pollInterval, awsAccountID, roleARN, externalID string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--namespace":
			if i+1 < len(args) {
				namespace = args[i+1]
				i++
			}
		case "--service":
			if i+1 < len(args) {
				service = args[i+1]
				i++
			}
		case "--log-group":
			if i+1 < len(args) {
				logGroup = args[i+1]
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

	if namespace == "" || service == "" || logGroup == "" || roleARN == "" {
		fmt.Fprintln(os.Stderr, `usage: weft source add --namespace NS --service NAME --log-group GROUP --role-arn ARN
    [--account AWS_ACCOUNT_ID] [--region REGION] [--interval DURATION] [--external-id ID]`)
		os.Exit(1)
	}
	if strings.Contains(service, "/") {
		fmt.Fprintln(os.Stderr, "--service should be the bare service name only; pass the namespace via --namespace")
		os.Exit(1)
	}
	if region == "" {
		region = "us-east-2"
	}
	if pollInterval == "" {
		pollInterval = "30s"
	}

	// Pull a live sample from the log group (the coordinator assumes the
	// cross-account role) and run the interactive schema wizard against it,
	// then commit. Same sample→detect→configure flow as weftd onboarding;
	// the schema lives server-side, no local file.
	fmt.Fprintf(os.Stderr, "Sampling recent events from %s…\n", logGroup)
	lines, err := client.SampleCloudWatchSource(shell.SampleCloudWatchSourceRequest{
		Namespace:    namespace,
		Service:      service,
		AWSAccountID: awsAccountID,
		RoleARN:      roleARN,
		ExternalID:   externalID,
		LogGroup:     logGroup,
		Region:       region,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "sample: %v\n", err)
		os.Exit(1)
	}
	if len(lines) == 0 {
		fmt.Fprintln(os.Stderr, "no events returned from the log group — nothing to sample")
		os.Exit(1)
	}

	schema, err := setup.RunSetupFromLines(namespace+"/"+service, lines)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup: %v\n", err)
		os.Exit(1)
	}

	if err := client.AddCloudWatchSource(shell.AddCloudWatchSourceRequest{
		Namespace:       namespace,
		Service:         service,
		AWSAccountID:    awsAccountID,
		RoleARN:         roleARN,
		ExternalID:      externalID,
		LogGroup:        logGroup,
		Region:          region,
		PollInterval:    pollInterval,
		Format:          schema.Format,
		TimestampField:  schema.TimestampField,
		TimestampFormat: schema.TimestampFormat,
		LevelField:      schema.LevelField,
		LevelPattern:    schema.LevelPattern,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "add source: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Added source: %s/%s → %s (account %s, poll every %s)\n", namespace, service, logGroup, awsAccountID, pollInterval)
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
	fmt.Printf("%-15s %-20s %-10s %-35s %-12s %s\n", "NAMESPACE", "SERVICE", "SOURCE", "LOG GROUP", "REGION", "LAST POLL")
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
		fmt.Fprintln(os.Stderr, "usage: weft source remove --namespace NS --service NAME")
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
		case "--service":
			if i+1 < len(args) {
				app = args[i+1]
				i++
			}
		}
	}
	if namespace == "" || app == "" {
		fmt.Fprintln(os.Stderr, "usage: weft source remove --namespace NS --service NAME")
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

// --- Integration subcommand ---

func runIntegration() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: weft integration {list|slack|pagerduty}")
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
	case "list", "ls":
		runIntegrationList(client)
	case "slack":
		runIntegrationSlack(client)
	case "pagerduty", "pd":
		runIntegrationPagerDuty(client)
	default:
		fmt.Fprintf(os.Stderr, "unknown integration command: %s\n", os.Args[2])
		fmt.Fprintln(os.Stderr, "usage: weft integration {list|slack|pagerduty}")
		os.Exit(1)
	}
}

func runIntegrationList(client *shell.HTTPCoordinator) {
	status, err := client.ListIntegrations()
	if err != nil {
		fmt.Fprintf(os.Stderr, "list integrations: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Slack")
	if status.Slack.Connected {
		name := status.Slack.TeamName
		if name == "" {
			name = "connected"
		}
		fmt.Printf("  connected: %s\n", name)
	} else {
		fmt.Println("  not connected — `weft integration slack connect`")
	}

	fmt.Println("\nPagerDuty")
	if len(status.PagerDuty.Integrations) == 0 {
		fmt.Println("  no integrations — `weft integration pagerduty add <name> --key <key>`")
		return
	}
	fmt.Printf("  %-20s %-16s %s\n", "NAME", "KEY", "LAST ERROR")
	for _, it := range status.PagerDuty.Integrations {
		fmt.Printf("  %-20s %-16s %s\n", it.Name, status.PagerDuty.Keys[it.Name], it.LastError)
	}
	fmt.Println("\nReference these names from /etc/notify.yaml under `pagerduty:`.")
}

func runIntegrationSlack(client *shell.HTTPCoordinator) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: weft integration slack {connect|rm}")
		os.Exit(1)
	}
	switch os.Args[3] {
	case "connect":
		runSlackConnect(client)
	case "rm", "disconnect":
		if err := client.SlackDisconnect(); err != nil {
			fmt.Fprintf(os.Stderr, "disconnect slack: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Slack disconnected.")
	default:
		fmt.Fprintf(os.Stderr, "unknown slack command: %s\n", os.Args[3])
		os.Exit(1)
	}
}

// runSlackConnect brokers the Slack OAuth flow terminal-side: it asks
// the server for an authorization URL, opens it, and polls the
// integrations status until the workspace shows connected. Slack OAuth
// fundamentally requires a browser redirect, but — like `weft login`'s
// device approval — the flow is initiated and confirmed from the shell.
func runSlackConnect(client *shell.HTTPCoordinator) {
	authURL, err := client.SlackConnectURL()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start slack connect: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Open this URL to authorize Slack for your workspace:\n\n  %s\n\n", authURL)
	openBrowser(authURL)
	fmt.Print("Waiting for authorization...")

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		fmt.Print(".")
		status, err := client.ListIntegrations()
		if err != nil {
			continue
		}
		if status.Slack.Connected {
			name := status.Slack.TeamName
			if name == "" {
				name = "your workspace"
			}
			fmt.Printf("\n\nSlack connected to %s.\n", name)
			fmt.Println("Route alerts to it from /etc/notify.yaml under `slack:`.")
			return
		}
	}
	fmt.Fprintf(os.Stderr, "\nTimed out waiting for Slack authorization.\n")
	os.Exit(1)
}

func runIntegrationPagerDuty(client *shell.HTTPCoordinator) {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: weft integration pagerduty {add|rm} ...")
		os.Exit(1)
	}
	switch os.Args[3] {
	case "add":
		args := os.Args[4:]
		var name, key string
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "--key":
				if i+1 < len(args) {
					key = args[i+1]
					i++
				}
			default:
				if !strings.HasPrefix(args[i], "-") && name == "" {
					name = args[i]
				}
			}
		}
		if name == "" || key == "" {
			fmt.Fprintln(os.Stderr, "usage: weft integration pagerduty add <name> --key <integration-key>")
			os.Exit(1)
		}
		if err := client.SavePagerDuty(name, key); err != nil {
			fmt.Fprintf(os.Stderr, "save pagerduty: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("PagerDuty integration %q saved. Reference it from /etc/notify.yaml under `pagerduty:`.\n", name)
	case "rm", "delete":
		if len(os.Args) < 5 {
			fmt.Fprintln(os.Stderr, "usage: weft integration pagerduty rm <name>")
			os.Exit(1)
		}
		name := os.Args[4]
		if err := client.DeletePagerDuty(name); err != nil {
			fmt.Fprintf(os.Stderr, "delete pagerduty: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("PagerDuty integration %q removed.\n", name)
	default:
		fmt.Fprintf(os.Stderr, "unknown pagerduty command: %s\n", os.Args[3])
		os.Exit(1)
	}
}

// openBrowser best-effort opens a URL in the user's default browser,
// trying macOS `open` then Linux `xdg-open`. Failure is silent — the
// URL is always printed for the user to open manually.
func openBrowser(url string) {
	if err := exec.Command("open", url).Start(); err != nil {
		exec.Command("xdg-open", url).Start()
	}
}

// --- Org subcommand ---

func runOrg() {
	args := os.Args[2:]
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list", "ls":
		runOrgList()
	case "switch", "use":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: weft org switch <name>")
			os.Exit(1)
		}
		runOrgSwitch(args[1])
	default:
		fmt.Fprintln(os.Stderr, "usage: weft org [list|switch <name>]")
		os.Exit(1)
	}
}

// fetchMe returns the /api/auth/me payload, including the orgs list.
// Used by both `weft org list` and `weft org switch` (the latter to
// validate the target name before persisting).
func fetchMe(cfg *shell.Config) (map[string]any, error) {
	req, err := http.NewRequest("GET", cfg.Endpoint+"/api/auth/me", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	if org := effectiveOrg(cfg); org != "" {
		req.Header.Set("X-Active-Org", org)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var me map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		return nil, err
	}
	return me, nil
}

func runOrgList() {
	cfg, err := shell.LoadConfig()
	if err != nil || cfg.Token == "" {
		fmt.Fprintln(os.Stderr, "Not logged in. Run 'weft login' to authenticate.")
		os.Exit(1)
	}
	me, err := fetchMe(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch orgs: %v\n", err)
		os.Exit(1)
	}
	orgs, _ := me["orgs"].([]any)
	if len(orgs) == 0 {
		fmt.Println("No org memberships.")
		return
	}
	fmt.Println("ACTIVE  NAME                 ROLE")
	for _, o := range orgs {
		m, _ := o.(map[string]any)
		marker := "        "
		if active, _ := m["active"].(bool); active {
			marker = "  *     "
		}
		fmt.Printf("%s%-20s %s\n", marker, m["name"], m["role"])
	}
}

func runOrgSwitch(name string) {
	cfg, err := shell.LoadConfig()
	if err != nil || cfg.Token == "" {
		fmt.Fprintln(os.Stderr, "Not logged in. Run 'weft login' to authenticate.")
		os.Exit(1)
	}
	me, err := fetchMe(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch orgs: %v\n", err)
		os.Exit(1)
	}
	orgs, _ := me["orgs"].([]any)
	found := false
	for _, o := range orgs {
		m, _ := o.(map[string]any)
		if n, _ := m["name"].(string); n == name {
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "not a member of %q. Run `weft org list` to see available orgs.\n", name)
		os.Exit(1)
	}
	cfg.Org = name
	if err := shell.SaveConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "save config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Switched to org: %s\n", name)
}
