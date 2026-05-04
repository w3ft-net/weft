package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kevlarcon/weft/cmd/weft/mcp/proposals"
	"github.com/kevlarcon/weft/shell"
)

// runPropose dispatches `weft propose <subcommand>`. The CLI is the
// human-side companion to the MCP propose tools: it reads the same
// on-disk staging dir an MCP server writes to, lets the human view
// diffs, and applies (or discards) the staged batch.
//
// Both processes — MCP server and `weft propose` — agree on the
// path defaultProposalsDir() so they share state without any
// runtime coordination.
func runPropose() {
	if len(os.Args) < 3 {
		proposeUsage()
		os.Exit(1)
	}
	switch os.Args[2] {
	case "review":
		runProposeReview()
	case "apply":
		runProposeApply()
	case "clear":
		runProposeClear()
	default:
		fmt.Fprintf(os.Stderr, "unknown propose subcommand: %s\n", os.Args[2])
		proposeUsage()
		os.Exit(1)
	}
}

func proposeUsage() {
	fmt.Fprintln(os.Stderr, "usage: weft propose {review|apply|clear} [options]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  review              show every staged proposal with its diff")
	fmt.Fprintln(os.Stderr, "  apply [--id ID]     apply all (or one) staged proposal(s)")
	fmt.Fprintln(os.Stderr, "  clear [--id ID]     discard all (or one) staged proposal(s)")
}

// defaultProposalsDir returns ~/.weft/proposals/current. Both the
// MCP server and the CLI agree on this path; running multiple MCP
// servers concurrently is not supported (the second one would
// share the first's staging area).
func defaultProposalsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".weft", "proposals", "current"), nil
}

func openProposalsStore() (*proposals.Store, error) {
	dir, err := defaultProposalsDir()
	if err != nil {
		return nil, err
	}
	return proposals.New(dir)
}

func runProposeReview() {
	store, err := openProposalsStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "open proposals: %v\n", err)
		os.Exit(1)
	}
	ps, err := store.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		os.Exit(1)
	}
	if len(ps) == 0 {
		fmt.Println("no proposals staged")
		return
	}
	fmt.Printf("%d proposal(s) staged in %s\n\n", len(ps), store.Dir())
	for _, p := range ps {
		fmt.Printf("───── %s  %s/%s%s ─────\n",
			p.ID, p.Kind, kindNamePrefix(p.SubKind), p.Name)
		if p.Rationale != "" {
			fmt.Printf("rationale: %s\n", p.Rationale)
		}
		fmt.Println()
		fmt.Println(p.Diff)
		fmt.Println()
	}
	fmt.Println("───── apply: weft propose apply       discard: weft propose clear ─────")
}

func kindNamePrefix(subKind string) string {
	if subKind == "" {
		return ""
	}
	return subKind + "/"
}

func runProposeApply() {
	fs := flag.NewFlagSet("propose apply", flag.ExitOnError)
	idFlag := fs.String("id", "", "apply only this proposal id; default applies all")
	_ = fs.Parse(os.Args[3:])

	cfg, err := shell.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Not logged in. Run 'weft login' to authenticate.")
		os.Exit(1)
	}
	client, err := httpClient(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	backend := shell.NewHTTPBackend(cfg.Endpoint, cfg.Token, effectiveOrg(cfg))
	_ = client // currently unused; reserved for tighter audit later

	store, err := openProposalsStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "open proposals: %v\n", err)
		os.Exit(1)
	}

	ps, err := store.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		os.Exit(1)
	}
	if *idFlag != "" {
		var filtered []proposals.Proposal
		for _, p := range ps {
			if p.ID == *idFlag {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) == 0 {
			fmt.Fprintf(os.Stderr, "no staged proposal with id %s\n", *idFlag)
			os.Exit(1)
		}
		ps = filtered
	}
	if len(ps) == 0 {
		fmt.Println("no proposals to apply")
		return
	}

	var applied, failed []string
	for _, p := range ps {
		if err := applyProposal(backend, p); err != nil {
			fmt.Fprintf(os.Stderr, "✗ %s %s/%s: %v\n", p.ID, p.Kind, p.Name, err)
			failed = append(failed, p.ID)
			break
		}
		fmt.Printf("✓ %s %s/%s\n", p.ID, p.Kind, p.Name)
		applied = append(applied, p.ID)
		if err := store.Remove(p.ID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: applied but failed to remove %s: %v\n", p.ID, err)
		}
	}

	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "\napply stopped at first failure (%s). %d applied, remaining still staged.\n",
			failed[0], len(applied))
		os.Exit(1)
	}
}

func runProposeClear() {
	fs := flag.NewFlagSet("propose clear", flag.ExitOnError)
	idFlag := fs.String("id", "", "discard only this proposal id; default discards all")
	_ = fs.Parse(os.Args[3:])

	store, err := openProposalsStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "open proposals: %v\n", err)
		os.Exit(1)
	}
	ps, _ := store.List()
	if len(ps) == 0 {
		fmt.Println("no proposals staged")
		return
	}

	target := func(p proposals.Proposal) bool { return *idFlag == "" || p.ID == *idFlag }
	removed := 0
	for _, p := range ps {
		if !target(p) {
			continue
		}
		if err := store.Remove(p.ID); err != nil {
			fmt.Fprintf(os.Stderr, "remove %s: %v\n", p.ID, err)
			os.Exit(1)
		}
		removed++
	}
	if *idFlag != "" && removed == 0 {
		fmt.Fprintf(os.Stderr, "no staged proposal with id %s\n", *idFlag)
		os.Exit(1)
	}
	fmt.Printf("discarded %d proposal(s)\n", removed)
}

// applyProposal commits one proposal to the backend. Crons land
// at /api/watches under the hood — "watch" is the legacy storage
// name for the same concept, scheduled for renaming separately.
func applyProposal(b *shell.HTTPBackend, p proposals.Proposal) error {
	switch p.Kind {
	case "script":
		return b.SaveFunction(p.Name, p.Proposed)
	case "cron":
		schedule, pipeline, err := parseCronBody(p.Proposed)
		if err != nil {
			return err
		}
		return b.SaveWatch(p.Name, &shell.WatchData{
			Schedule: schedule,
			Pipeline: pipeline,
		})
	case "dashboard":
		return b.SaveDashboard(p.Name, p.Proposed)
	case "delete":
		switch p.SubKind {
		case "script":
			return b.DeleteFunction(p.Name)
		case "cron":
			return b.DeleteWatch(p.Name)
		case "dashboard":
			return b.DeleteDashboard(p.Name)
		default:
			return fmt.Errorf("unknown delete sub-kind %q", p.SubKind)
		}
	default:
		return fmt.Errorf("unknown kind %q", p.Kind)
	}
}

// parseCronBody reads the canonical cron-proposal body shape:
//
//	schedule: 0 2 * * *
//	pipeline: cat /log/...
func parseCronBody(body string) (schedule, pipeline string, err error) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.TrimSpace(key) {
		case "schedule":
			schedule = val
		case "pipeline":
			pipeline = val
		}
	}
	if schedule == "" || pipeline == "" {
		return "", "", errors.New("malformed cron proposal: schedule + pipeline required")
	}
	return schedule, pipeline, nil
}
