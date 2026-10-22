package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/cmd/weft/mcp/policy"
	"github.com/graybeardcorp/weft/cmd/weft/mcp/proposals"
)

// The server instructions and tool descriptions are the only manual an
// assistant reads before it writes a pipeline or a cron. They drifted from the
// product once (old path layout, "5-field cron expression", "watch"), so these
// tests pin the facts that matter and the stale phrases that must not return.

func descriptionsSession(t *testing.T) (instructions string, tools map[string]string) {
	t.Helper()
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{}).wire(&cfg)
	s := testSession(t, cfg)

	instructions = s.InitializeResult().Instructions
	list, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tools = map[string]string{}
	for _, tool := range list.Tools {
		tools[tool.Name] = tool.Description
	}
	return instructions, tools
}

func mustContain(t *testing.T, what, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("%s should mention %q", what, w)
		}
	}
}

func TestServerInstructions_DescribeTheCurrentProduct(t *testing.T) {
	instructions, tools := descriptionsSession(t)

	mustContain(t, "instructions", instructions,
		"<weft-data>",                      // the prompt-injection defense stays first-class
		"/log/<namespace>/<service>/",      // 3-segment path: every env
		"/log/<namespace>/<env>/<service>", // 4-segment path: one env
		"weft::describe",                   // how to read cron history
		"weft.dashboard_propose",           // every proposal tool is named
		"weft.proposal_delete",
		"weft.proposals_apply",
	)
	for name := range tools {
		if !strings.Contains(instructions, name) {
			t.Errorf("instructions never name the tool %s", name)
		}
	}
}

func TestToolDescriptions_AreCurrent(t *testing.T) {
	_, tools := descriptionsSession(t)

	mustContain(t, "weft.ls", tools["weft.ls"], "<env>")
	mustContain(t, "weft.cat", tools["weft.cat"], "/log/")
	mustContain(t, "weft.run", tools["weft.run"], "weft::", "/log/billing/prod/api/main.log")
	mustContain(t, "weft.crons_list", tools["weft.crons_list"], "every 5m")

	mustContain(t, "weft.cron_propose", tools["weft.cron_propose"],
		"every 5m", "0 0 * * 1", "UTC", // the schedule grammar
		"head -n",                     // never cap a cron's rows
		"rising", "absent", "changed", // the full condition set
		"--notify", "-v", "-t", // alert options
		"final stage",                 // alert position
		"weft::is_above", "echo fire", // the composite form
	)
	mustContain(t, "weft.script_propose", tools["weft.script_propose"], "final stage", "weft::")
	mustContain(t, "weft.dashboard_propose", tools["weft.dashboard_propose"], "cron:")
}

// Every example in weft.run's description is a pipeline the tool accepts, and
// the composite example in weft.cron_propose passes the cron policy. An
// example the validator refuses teaches the assistant to fail.
func TestToolDescriptions_ExamplesPassThePolicy(t *testing.T) {
	for _, line := range strings.Split(runDescription, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		pipeline := strings.TrimSpace(line)
		if _, vs := policy.Validate(pipeline, policy.DefaultReadPolicy()); len(vs) != 0 {
			t.Errorf("weft.run example %q is refused: %+v", pipeline, vs)
		}
	}

	const composite = "(weft::is_above error-rate 50 && weft::is_absent canary-ping && echo fire) | alert --notify=critical"
	if !strings.Contains(cronProposeDescription, composite) {
		t.Fatal("weft.cron_propose no longer shows the composite example this test checks")
	}
	if _, vs := policy.Validate(composite, policy.DefaultCronPolicy()); len(vs) != 0 {
		t.Errorf("composite example is refused: %+v", vs)
	}
	const number = "weft::avg error-rate 5m | alert above 50 --notify=ops"
	if !strings.Contains(cronProposeDescription, number) {
		t.Fatal("weft.cron_propose no longer shows the number example this test checks")
	}
	if _, vs := policy.Validate(number, policy.DefaultCronPolicy()); len(vs) != 0 {
		t.Errorf("number example is refused: %+v", vs)
	}
}
