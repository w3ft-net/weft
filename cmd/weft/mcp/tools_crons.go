package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CronsListArgs is intentionally empty: crons_list takes no input, it
// returns every cron for the authenticated customer. The SDK still
// needs a struct to derive the (empty) input schema from.
type CronsListArgs struct{}

const cronsListDescription = `List every scheduled cron for the authenticated customer, with its schedule and full pipeline.

Read-only. Use this to audit what's scheduled — e.g. finding crons whose pipelines reference stale log paths, or recovering a cron's exact schedule/pipeline before proposing a corrected version via weft.cron_propose.

Output is plain text, one block per cron:
  name: <name>
  schedule: <5-field cron expression>
  pipeline: <the pipeline, including any | alert ... stage>`

func registerCronsList(server *mcp.Server, cfg Config) {
	mcp.AddTool(server,
		&mcp.Tool{Name: "weft.crons_list", Description: cronsListDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ CronsListArgs) (*mcp.CallToolResult, any, error) {
			return cronsListHandler(cfg), nil, nil
		},
	)
}

// cronsListHandler composes the already-wired ListWatches (names) and
// GetCron (per-cron schedule+pipeline body) hooks. It deliberately
// adds no new Config surface or backend call: the same /api/watches
// endpoints that back cron_propose's diff already expose everything.
func cronsListHandler(cfg Config) *mcp.CallToolResult {
	if cfg.ListWatches == nil || cfg.GetCron == nil {
		return errorResult("weft.crons_list is not configured on this server")
	}

	names, err := cfg.ListWatches()
	if err != nil {
		return errorResult(fmt.Sprintf("list crons: %v", err))
	}
	if len(names) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "no crons defined"}},
		}
	}
	sort.Strings(names)

	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "name: %s\n", name)
		// GetCron returns "schedule: ...\npipeline: ...\n"; production
		// yields ("", nil) for a name that vanished between the list
		// and the get (e.g. concurrent delete) — surface it rather
		// than silently dropping the row.
		body, err := cfg.GetCron(name)
		switch {
		case err != nil:
			fmt.Fprintf(&b, "(error: %v)\n", err)
		case strings.TrimSpace(body) == "":
			b.WriteString("(unavailable)\n")
		default:
			b.WriteString(body)
			if !strings.HasSuffix(body, "\n") {
				b.WriteString("\n")
			}
		}
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: strings.TrimRight(b.String(), "\n")}},
	}
}
