package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/graybeardcorp/weft/cmd/weft/mcp/policy"
	"github.com/graybeardcorp/weft/cmd/weft/mcp/proposals"
	"github.com/graybeardcorp/weft/store"
)

// ScriptProposeArgs / CronProposeArgs / WatchProposeArgs /
// ProposalDeleteArgs are the wire schemas the SDK reflects into
// JSONSchema for each tool. Field names and json tags become the
// interface contract the LLM sees.

type ScriptProposeArgs struct {
	Name      string `json:"name" jsonschema:"name of the script (lands at bin/<name>)"`
	Body      string `json:"body" jsonschema:"the script body — a single pipeline using $1, $@, etc."`
	Rationale string `json:"rationale,omitempty" jsonschema:"one-sentence reason for the change; shown alongside the diff at review time"`
}

type CronProposeArgs struct {
	Name      string `json:"name" jsonschema:"name of the cron entry (lands at cron.d/<name>)"`
	Schedule  string `json:"schedule" jsonschema:"a crontab such as '*/5 * * * *', '0 * * * *', '0 0 * * *' (daily) or '0 0 * * 1' (weekly, Monday), or an interval such as 'every 5m' or 'every 1d'; crons fire at UTC-aligned intervals, not at a chosen time of day"`
	Pipeline  string `json:"pipeline" jsonschema:"the pipeline to run; same allowlist as weft.run"`
	Rationale string `json:"rationale,omitempty"`
}

type DashboardProposeArgs struct {
	Name      string `json:"name" jsonschema:"name of the dashboard (lands at /etc/dashboards/<name>.yaml)"`
	Body      string `json:"body" jsonschema:"the dashboard YAML — sections + tiles, see manual/dashboards.md"`
	Rationale string `json:"rationale,omitempty" jsonschema:"one-sentence reason for the change; shown alongside the diff at review time"`
}

type ProposalDeleteArgs struct {
	Kind string `json:"kind" jsonschema:"one of: script | cron | dashboard"`
	Name string `json:"name" jsonschema:"name of the resource to remove"`
}

const (
	scriptProposeDescription = `Stage a new (or updated) bin/ script for human review.

The body is parsed against weft's script policy: pure-pipeline form, allowed verbs only, paths under /log/ or positional ($1/$@). No redirects, command substitution, subshells, or function definitions.

Returns the proposal id and a unified diff against the current script (if any). The change is NOT committed — call weft.proposals_apply (which the user must approve) to land it.`

	cronProposeDescription = `Stage a new (or updated) cron entry for human review.

A cron is a scheduled pipeline. To make it an alert, end the pipeline with '| alert <condition>' (e.g. 'alert above 50', 'alert below 10', 'alert eq 0'). The schedule must be a 5-field cron expression.

Pipelines pass the same allowlist as weft.run plus the alert pipe stage. Returns proposal id + unified diff.`

	dashboardProposeDescription = `Stage a new (or updated) dashboard for human review.

The body is the dashboard YAML — top-level title plus a list of sections (each with a heading and tiles), or a flat top-level tiles: shorthand. Tiles have a kind (sparkline | counter | gauge | table), a watch reference, an optional size hint (sm | md | lg), and kind-specific fields like format, threshold, or columns.

The body is parsed against weft's dashboard YAML grammar and every tile's cron: reference is checked against the existing crons. Returns proposal id + unified diff.`

	proposalDeleteDescription = `Stage the deletion of an existing script, cron, or dashboard.

The current contents are recorded so the human can see exactly what is being removed in the review. Errors if the resource doesn't exist.`
)

func registerPropose(server *mcp.Server, cfg Config) {
	mcp.AddTool(server,
		&mcp.Tool{Name: "weft.script_propose", Description: scriptProposeDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, a ScriptProposeArgs) (*mcp.CallToolResult, any, error) {
			return scriptProposeHandler(cfg, a), nil, nil
		},
	)
	mcp.AddTool(server,
		&mcp.Tool{Name: "weft.cron_propose", Description: cronProposeDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, a CronProposeArgs) (*mcp.CallToolResult, any, error) {
			return cronProposeHandler(cfg, a), nil, nil
		},
	)
	mcp.AddTool(server,
		&mcp.Tool{Name: "weft.dashboard_propose", Description: dashboardProposeDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, a DashboardProposeArgs) (*mcp.CallToolResult, any, error) {
			return dashboardProposeHandler(cfg, a), nil, nil
		},
	)
	mcp.AddTool(server,
		&mcp.Tool{Name: "weft.proposal_delete", Description: proposalDeleteDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, a ProposalDeleteArgs) (*mcp.CallToolResult, any, error) {
			return proposalDeleteHandler(cfg, a), nil, nil
		},
	)
}

func scriptProposeHandler(cfg Config, a ScriptProposeArgs) *mcp.CallToolResult {
	if a.Name == "" || a.Body == "" {
		return errorResult("name and body are required")
	}
	if _, vs := policy.Validate(a.Body, policy.DefaultScriptPolicy()); len(vs) > 0 {
		return errorResult(formatViolations(vs))
	}
	current, err := getCurrent(cfg.GetScript, a.Name)
	if err != nil {
		return errorResult(fmt.Sprintf("fetch current script: %v", err))
	}
	return stageProposal(cfg.Proposals, proposals.Proposal{
		Kind:      "script",
		Name:      a.Name,
		Rationale: a.Rationale,
		Current:   current,
		Proposed:  a.Body,
	})
}

func cronProposeHandler(cfg Config, a CronProposeArgs) *mcp.CallToolResult {
	if a.Name == "" || a.Schedule == "" || a.Pipeline == "" {
		return errorResult("name, schedule, and pipeline are required")
	}
	if _, err := store.ValidateSchedule(strings.TrimSpace(a.Schedule)); err != nil {
		return errorResult(err.Error())
	}
	// Two complementary validators. DefaultCronPolicy enforces the
	// allowlist + path scoping (so `rm /log/x | alert above 1`
	// can't slip through); CompileWatchPipeline enforces alert
	// shape (at most one alert; valid -v / -t args). Both run so
	// the model sees every off-policy fact at once.
	//
	// User-defined bin/ scripts are looked up fresh per-call: a
	// script proposed and applied earlier in the same session is
	// usable as a cron command immediately, with no MCP restart.
	pol := policy.DefaultCronPolicy()
	if cfg.ListScripts != nil {
		if names, err := cfg.ListScripts(); err == nil && len(names) > 0 {
			pol.UserDefinedCommands = make(map[string]bool, len(names))
			for _, n := range names {
				pol.UserDefinedCommands[n] = true
			}
		}
	}
	if _, vs := policy.Validate(a.Pipeline, pol); len(vs) > 0 {
		return errorResult(formatViolations(vs))
	}
	if _, err := store.CompileWatchPipeline(a.Pipeline); err != nil {
		return errorResult(fmt.Sprintf("cron pipeline rejected: %v", err))
	}
	current, err := getCurrent(cfg.GetCron, a.Name)
	if err != nil {
		return errorResult(fmt.Sprintf("fetch current cron: %v", err))
	}
	body := fmt.Sprintf("schedule: %s\npipeline: %s\n", a.Schedule, a.Pipeline)
	return stageProposal(cfg.Proposals, proposals.Proposal{
		Kind:      "cron",
		Name:      a.Name,
		Rationale: a.Rationale,
		Current:   current,
		Proposed:  body,
	})
}

func dashboardProposeHandler(cfg Config, a DashboardProposeArgs) *mcp.CallToolResult {
	if a.Name == "" || a.Body == "" {
		return errorResult("name and body are required")
	}
	spec, err := store.ParseDashboardYAML(a.Body)
	if err != nil {
		return errorResult(fmt.Sprintf("dashboard yaml rejected: %v", err))
	}
	// Watch-ref check uses the same population pattern as
	// cron_propose's UserDefinedCommands: pull a fresh list per
	// call, so a watch proposed and applied earlier in the same
	// session is usable as a tile target immediately. When
	// ListWatches is unset we skip the check rather than falsely
	// failing — propose still stages, the human review catches
	// dangling refs.
	if cfg.ListWatches != nil {
		names, err := cfg.ListWatches()
		if err != nil {
			return errorResult(fmt.Sprintf("list crons: %v", err))
		}
		known := make(map[string]bool, len(names))
		for _, n := range names {
			known[n] = true
		}
		var missing []string
		seen := map[string]bool{}
		for _, sec := range spec.Sections {
			for _, tile := range sec.Tiles {
				if seen[tile.Watch] {
					continue
				}
				seen[tile.Watch] = true
				if !known[tile.Watch] {
					missing = append(missing, tile.Watch)
				}
			}
		}
		if len(missing) > 0 {
			return errorResult(fmt.Sprintf("dashboard references unknown crons: %s", strings.Join(missing, ", ")))
		}
	}
	current, err := getCurrent(cfg.GetDashboard, a.Name)
	if err != nil {
		return errorResult(fmt.Sprintf("fetch current dashboard: %v", err))
	}
	return stageProposal(cfg.Proposals, proposals.Proposal{
		Kind:      "dashboard",
		Name:      a.Name,
		Rationale: a.Rationale,
		Current:   current,
		Proposed:  a.Body,
	})
}

func proposalDeleteHandler(cfg Config, a ProposalDeleteArgs) *mcp.CallToolResult {
	if a.Kind == "" || a.Name == "" {
		return errorResult("kind and name are required")
	}
	getter := getterFor(cfg, a.Kind)
	if getter == nil {
		return errorResult(fmt.Sprintf("unknown kind %q (want script|cron|dashboard)", a.Kind))
	}
	current, err := getter(a.Name)
	if err != nil {
		return errorResult(fmt.Sprintf("fetch current %s: %v", a.Kind, err))
	}
	if current == "" {
		// A delete that isn't deleting anything would still
		// successfully stage but be a no-op when applied. Block it
		// at propose time so the model doesn't spam the proposal
		// list with phantom deletes.
		return errorResult(fmt.Sprintf("%s %q does not exist", a.Kind, a.Name))
	}
	return stageProposal(cfg.Proposals, proposals.Proposal{
		Kind:    "delete",
		SubKind: a.Kind,
		Name:    a.Name,
		Current: current,
	})
}

// getCurrent calls f and converts a nil function into a benign empty
// string + nil error. Tests that don't care about diff shape get to
// leave the getter unwired.
func getCurrent(f GetterFunc, name string) (string, error) {
	if f == nil {
		return "", nil
	}
	return f(name)
}

func getterFor(cfg Config, kind string) GetterFunc {
	switch kind {
	case "script":
		return cfg.GetScript
	case "cron":
		return cfg.GetCron
	case "dashboard":
		return cfg.GetDashboard
	}
	return nil
}

// stageProposal is the common tail for every successful propose
// tool: write to disk, return id + diff to the model so it can
// summarise the staged batch in chat.
func stageProposal(s *proposals.Store, p proposals.Proposal) *mcp.CallToolResult {
	id, err := s.Add(p)
	if err != nil {
		return errorResult(fmt.Sprintf("stage proposal: %v", err))
	}
	saved, err := s.Get(id)
	if err != nil {
		return errorResult(fmt.Sprintf("read back proposal: %v", err))
	}
	body := fmt.Sprintf("proposal %s staged (%s/%s)\n\n%s", id, saved.Kind, saved.Name, saved.Diff)
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: body}},
	}
}
