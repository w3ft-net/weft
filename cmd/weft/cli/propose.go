package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/w3ft-net/weft/cmd/weft/mcp/proposals"
	"github.com/w3ft-net/weft/cmd/weft/ui"
	"github.com/w3ft-net/weft/shell"
	"github.com/w3ft-net/weft/warning"
)

// proposeCmd is the human side of the MCP propose tools: it reads the staging
// dir an MCP server writes to, shows the diffs, and applies or discards the
// staged batch. Both processes agree on defaultProposalsDir, so they share
// state without any runtime coordination.
func proposeCmd(e Env) *cobra.Command {
	cmd := &cobra.Command{Use: "propose", Short: "Review or apply MCP-staged mutations (review|apply|clear)"}

	cmd.AddCommand(&cobra.Command{
		Use: "review", Short: "Show every staged proposal with its diff", Args: cobra.NoArgs,
		RunE: runE(func(*cobra.Command, []string) error {
			store, err := openProposalsStore()
			if err != nil {
				return fmt.Errorf("open proposals: %v", err)
			}
			ps, err := store.List()
			if err != nil {
				return fmt.Errorf("list: %v", err)
			}
			if len(ps) == 0 {
				ui.Hintln("no proposals staged")
				return nil
			}
			st := ui.Out()
			fmt.Printf("%s\n\n", st.Muted.Render(fmt.Sprintf("%d proposal(s) staged in %s", len(ps), store.Dir())))
			for _, p := range ps {
				fmt.Println(st.Head.Render(fmt.Sprintf("───── %s  %s/%s%s ─────",
					p.ID, p.Kind, kindNamePrefix(p.SubKind), p.Name)))
				if p.Rationale != "" {
					fmt.Printf("%s %s\n", st.Muted.Render("rationale:"), p.Rationale)
				}
				fmt.Println()
				fmt.Println(st.ColorDiff(p.Diff))
				fmt.Println()
			}
			fmt.Println(st.Head.Render("───── apply: weft propose apply       discard: weft propose clear ─────"))
			return nil
		}),
	})

	var applyID string
	apply := &cobra.Command{
		Use: "apply [--id ID]", Short: "Apply all (or one) staged proposal(s)", Args: cobra.NoArgs,
		RunE: runE(func(*cobra.Command, []string) error {
			backend, err := e.Backend()
			if err != nil {
				return err
			}
			store, err := openProposalsStore()
			if err != nil {
				return fmt.Errorf("open proposals: %v", err)
			}
			ps, err := store.List()
			if err != nil {
				return fmt.Errorf("list: %v", err)
			}
			if applyID != "" {
				var filtered []proposals.Proposal
				for _, p := range ps {
					if p.ID == applyID {
						filtered = append(filtered, p)
					}
				}
				if len(filtered) == 0 {
					return fmt.Errorf("no staged proposal with id %s", applyID)
				}
				ps = filtered
			}
			if len(ps) == 0 {
				ui.Hintln("no proposals to apply")
				return nil
			}
			applied, failed := applyStaged(ps,
				func(p proposals.Proposal) error { return applyProposal(backend, p) },
				store.Remove)
			if len(failed) > 0 {
				ui.Errorf("\napply stopped at first failure (%s). %d applied, remaining still staged.\n", failed[0], len(applied))
				return errReported
			}
			return nil
		}),
	}
	apply.Flags().StringVar(&applyID, "id", "", "Apply only this proposal id")
	cmd.AddCommand(apply)

	var clearID string
	clear := &cobra.Command{
		Use: "clear [--id ID]", Short: "Discard all (or one) staged proposal(s)", Args: cobra.NoArgs,
		RunE: runE(func(*cobra.Command, []string) error {
			store, err := openProposalsStore()
			if err != nil {
				return fmt.Errorf("open proposals: %v", err)
			}
			ps, _ := store.List()
			if len(ps) == 0 {
				ui.Hintln("no proposals staged")
				return nil
			}
			removed := 0
			for _, p := range ps {
				if clearID != "" && p.ID != clearID {
					continue
				}
				if err := store.Remove(p.ID); err != nil {
					return fmt.Errorf("remove %s: %v", p.ID, err)
				}
				removed++
			}
			if clearID != "" && removed == 0 {
				return fmt.Errorf("no staged proposal with id %s", clearID)
			}
			ui.Successf("discarded %d proposal(s)\n", removed)
			return nil
		}),
	}
	clear.Flags().StringVar(&clearID, "id", "", "Discard only this proposal id")
	cmd.AddCommand(clear)
	return cmd
}

func openProposalsStore() (*proposals.Store, error) {
	dir, err := defaultProposalsDir()
	if err != nil {
		return nil, err
	}
	return proposals.New(dir)
}

func kindNamePrefix(subKind string) string {
	if subKind == "" {
		return ""
	}
	return subKind + "/"
}

// applyStaged applies ps in order and unstages each one that lands. A save that
// comes back with only warnings landed (the server stored it and has something
// to say), so it gets a ⚠ and the queue carries on; a refusal gets a ✗ and
// stops the queue, leaving it and the rest staged.
func applyStaged(ps []proposals.Proposal, apply func(proposals.Proposal) error, remove func(id string) error) (applied, failed []string) {
	for _, p := range ps {
		blocking, warnings := warning.Split(apply(p))
		if len(blocking) > 0 {
			ui.Errorf("✗ %s %s/%s: %v\n", p.ID, p.Kind, p.Name, errors.Join(blocking...))
			return applied, append(failed, p.ID)
		}
		if len(warnings) > 0 {
			ui.Warnf("⚠ %s %s/%s: %s\n", p.ID, p.Kind, p.Name, strings.Join(warnings, "\n"))
		} else {
			ui.Successf("✓ %s %s/%s\n", p.ID, p.Kind, p.Name)
		}
		applied = append(applied, p.ID)
		if err := remove(p.ID); err != nil {
			ui.Warnf("warning: applied but failed to remove %s: %v\n", p.ID, err)
		}
	}
	return applied, failed
}

// applyProposal commits one proposal to the backend. Crons land at /api/crons
// under the hood ("watch" is the legacy storage name for the same concept).
func applyProposal(b *shell.HTTPBackend, p proposals.Proposal) error {
	switch p.Kind {
	case "script":
		return b.SaveFunction(p.Name, p.Proposed)
	case "cron":
		schedule, pipeline, err := parseCronBody(p.Proposed)
		if err != nil {
			return err
		}
		return b.SaveWatch(p.Name, &shell.WatchData{Schedule: schedule, Pipeline: pipeline})
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
//	schedule: 0 0 * * *
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
