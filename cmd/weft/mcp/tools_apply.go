package mcp

import (
	"context"
	"errors"
	"fmt"
	"github.com/w3ft-net/weft/warning"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/w3ft-net/weft/cmd/weft/mcp/proposals"
)

// ProposalsListArgs / DiffArgs / ApplyArgs are the wire schemas
// for the apply-side tools.
type ProposalsListArgs struct{}

type ProposalsDiffArgs struct {
	ID string `json:"id" jsonschema:"the proposal id (e.g. 0001) returned by a *_propose call"`
}

type ProposalsApplyArgs struct {
	// IDs is the list of staged proposals to commit, in order.
	// Apply walks the list head-to-tail; the first failure stops
	// the batch and leaves the failing + remaining proposals
	// staged so the human can review what changed.
	IDs []string `json:"ids" jsonschema:"the proposal ids to apply, in order"`
}

const (
	listDescription = `List every proposal currently staged in this session.

Returns one line per proposal: id, kind, name, and a one-line summary so the model can show the human what's pending. Use weft.proposals_diff for the full unified diff of one entry.`

	diffDescription = `Return the unified diff for a single staged proposal.

Useful when re-presenting a specific change before applying. The diff is the same one weft.<kind>_propose returned at staging time.`

	applyDescription = `Commit one or more staged proposals to the backend.

CRITICAL: this is the only tool that mutates state. The host should require an explicit user-side approval for this call. The tool itself walks the supplied ids in order; on the first failure it stops and reports which proposals landed, which failed, and which were skipped. Successful applies are removed from the staging store; failures and skips remain so the human can re-review.`
)

func registerApply(server *mcp.Server, cfg Config) {
	mcp.AddTool(server,
		&mcp.Tool{Name: "weft.proposals_list", Description: listDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ ProposalsListArgs) (*mcp.CallToolResult, any, error) {
			return proposalsListHandler(cfg), nil, nil
		},
	)
	mcp.AddTool(server,
		&mcp.Tool{Name: "weft.proposals_diff", Description: diffDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, a ProposalsDiffArgs) (*mcp.CallToolResult, any, error) {
			return proposalsDiffHandler(cfg, a), nil, nil
		},
	)
	mcp.AddTool(server,
		&mcp.Tool{Name: "weft.proposals_apply", Description: applyDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, a ProposalsApplyArgs) (*mcp.CallToolResult, any, error) {
			return proposalsApplyHandler(cfg, a), nil, nil
		},
	)
}

func proposalsListHandler(cfg Config) *mcp.CallToolResult {
	ps, err := cfg.Proposals.List()
	if err != nil {
		return errorResult(fmt.Sprintf("list: %v", err))
	}
	if len(ps) == 0 {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "no proposals staged"}}}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-6s %-8s %-12s %s\n", "ID", "KIND", "SUBKIND", "NAME")
	for _, p := range ps {
		fmt.Fprintf(&b, "%-6s %-8s %-12s %s\n", p.ID, p.Kind, p.SubKind, p.Name)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}}}
}

func proposalsDiffHandler(cfg Config, a ProposalsDiffArgs) *mcp.CallToolResult {
	if a.ID == "" {
		return errorResult("id is required")
	}
	p, err := cfg.Proposals.Get(a.ID)
	if err != nil {
		return errorResult(err.Error())
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: p.Diff}}}
}

// proposalsApplyHandler commits each requested proposal in order.
// The pre-flight Get() loop is what makes "unknown id fails the
// whole batch" work: we surface the bad id before any backend
// mutation rather than half-applying. Once the pre-flight passes,
// each Save / Delete is attempted in turn; the first error stops
// the loop and the response details exactly what landed.
func proposalsApplyHandler(cfg Config, a ProposalsApplyArgs) *mcp.CallToolResult {
	if len(a.IDs) == 0 {
		return errorResult("ids must be a non-empty list")
	}

	// Pre-flight: load every proposal, fail the whole batch if any
	// id is unknown. Saves us from "applied half, then crashed."
	props := make([]proposals.Proposal, 0, len(a.IDs))
	for _, id := range a.IDs {
		p, err := cfg.Proposals.Get(id)
		if err != nil {
			return errorResult(fmt.Sprintf("apply: %s: %v", id, err))
		}
		props = append(props, p)
	}

	// applyOne's error may be a non-blocking warning.Warning (or an
	// errors.Join of several) — e.g. the /cron.d truncation lint.
	// store.Split separates that from an actual rejection: a
	// Warning-only result still counts as applied, with its message
	// collected for display; only a real blocking error stops the
	// batch. Generic — this doesn't know or care which check (if any)
	// produced a given warning.
	var applied, failed, skipped, warningLines []string
	var firstErr error
	for i, p := range props {
		if firstErr != nil {
			skipped = append(skipped, p.ID)
			continue
		}
		blocking, warnings := warning.Split(applyOne(cfg, p))
		if len(blocking) > 0 {
			firstErr = errors.Join(blocking...)
			failed = append(failed, p.ID)
			// Subsequent ids are skipped (i+1..end).
			for _, rest := range props[i+1:] {
				skipped = append(skipped, rest.ID)
			}
			break
		}
		for _, w := range warnings {
			warningLines = append(warningLines, fmt.Sprintf("%s: %s", p.ID, w))
		}
		applied = append(applied, p.ID)
		// Drop the staged file once the backend mutation succeeded.
		// A Remove failure here is non-fatal — the proposal landed,
		// the worst case is the human sees a stale entry and clears
		// it manually.
		_ = cfg.Proposals.Remove(p.ID)
	}

	if firstErr != nil {
		body := fmt.Sprintf("apply failed at %s: %v\n\napplied: %v\nfailed: %v\nskipped: %v",
			failed[0], firstErr, applied, failed, skipped)
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: body}}}
	}
	text := fmt.Sprintf("applied %d proposal(s): %v", len(applied), applied)
	if len(warningLines) > 0 {
		text += "\n\nwarnings:\n" + strings.Join(warningLines, "\n")
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// applyOne dispatches to the correct backend function based on
// kind. Kinds the server can stage but cannot apply are caught
// here so a malformed proposal file (corruption, manual edit)
// surfaces as a clean error rather than a silent no-op.
func applyOne(cfg Config, p proposals.Proposal) error {
	switch p.Kind {
	case "script":
		if cfg.SaveScript == nil {
			return fmt.Errorf("SaveScript not configured")
		}
		return cfg.SaveScript(p.Name, p.Proposed)
	case "cron":
		if cfg.SaveCron == nil {
			return fmt.Errorf("SaveCron not configured")
		}
		return cfg.SaveCron(p.Name, p.Proposed)
	case "dashboard":
		if cfg.SaveDashboard == nil {
			return fmt.Errorf("SaveDashboard not configured")
		}
		return cfg.SaveDashboard(p.Name, p.Proposed)
	case "delete":
		switch p.SubKind {
		case "script":
			if cfg.DeleteScript == nil {
				return fmt.Errorf("DeleteScript not configured")
			}
			return cfg.DeleteScript(p.Name)
		case "cron":
			if cfg.DeleteCron == nil {
				return fmt.Errorf("DeleteCron not configured")
			}
			return cfg.DeleteCron(p.Name)
		case "dashboard":
			if cfg.DeleteDashboard == nil {
				return fmt.Errorf("DeleteDashboard not configured")
			}
			return cfg.DeleteDashboard(p.Name)
		default:
			return fmt.Errorf("unknown delete sub-kind %q", p.SubKind)
		}
	default:
		return fmt.Errorf("unknown kind %q", p.Kind)
	}
}
