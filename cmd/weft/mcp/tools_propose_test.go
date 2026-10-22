package mcp

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/cmd/weft/mcp/proposals"
)

// proposeBackend is the stub used across propose-tool tests.
// Each map represents one of the resource namespaces; the tool
// looks up "current" contents via the matching Get function.
type proposeBackend struct {
	scripts    map[string]string
	crons      map[string]string
	dashboards map[string]string
	watches    []string // names that ListWatches should return
}

func (b *proposeBackend) wire(cfg *Config) {
	cfg.GetScript = func(name string) (string, error) { return b.scripts[name], nil }
	cfg.GetCron = func(name string) (string, error) { return b.crons[name], nil }
	cfg.GetDashboard = func(name string) (string, error) { return b.dashboards[name], nil }
	cfg.ListScripts = func() ([]string, error) {
		out := make([]string, 0, len(b.scripts))
		for name := range b.scripts {
			out = append(out, name)
		}
		return out, nil
	}
	cfg.ListWatches = func() ([]string, error) {
		return b.watches, nil
	}
}

// freshSession sets up a session whose Config has propose tools
// wired against a temp-dir store and stub backends. Returns the
// session and the underlying store so tests can inspect what
// landed on disk.
func freshSession(t *testing.T, b *proposeBackend) (*sessionPair, *proposals.Store) {
	t.Helper()
	store, err := proposals.New(t.TempDir())
	if err != nil {
		t.Fatalf("proposals.New: %v", err)
	}
	cfg := Config{
		Run:       neverRunFn(t),
		Proposals: store,
	}
	b.wire(&cfg)
	return &sessionPair{Client: testSession(t, cfg)}, store
}

// sessionPair lets tests treat the in-memory client like a small
// API — `s.Client.CallTool` reads naturally inline. Wrapped so we
// can grow it later without renaming every test.
type sessionPair struct {
	Client interface {
		// CallTool is exposed via the mcp.ClientSession returned
		// from testSession; we keep this unexported helper purely
		// for clarity.
	}
}

// TestScriptPropose_HappyPath: a valid script body lands as a
// proposal with id and unified diff. The current backend returns
// nothing (no existing script), so the diff is a pure addition.
func TestScriptPropose_HappyPath(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{}).wire(&cfg)
	s := testSession(t, cfg)

	res := callTool(t, s, "weft.script_propose", map[string]any{
		"name":      "err-rate-by-window",
		"body":      "cat $1 | jq -r .level | uniq -c\n",
		"rationale": "extracted from 3 watches",
	})
	if res.IsError {
		t.Fatalf("unexpected IsError; content=%q", textContent(res))
	}
	got := textContent(res)
	if !strings.Contains(got, "0001") {
		t.Errorf("expected proposal id in result; got %q", got)
	}
	if !strings.Contains(got, "+cat $1 | jq -r .level") {
		t.Errorf("expected unified diff in result; got %q", got)
	}

	// Verify it actually persisted.
	ps, _ := store.List()
	if len(ps) != 1 {
		t.Fatalf("expected 1 staged proposal, got %d", len(ps))
	}
	if ps[0].Name != "err-rate-by-window" || ps[0].Kind != "script" {
		t.Errorf("staged proposal mismatch: %+v", ps[0])
	}
}

// TestScriptPropose_PolicyViolation: a body that doesn't pass
// DefaultScriptPolicy never reaches the store. The model gets a
// structured error so it can revise.
func TestScriptPropose_PolicyViolation(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{}).wire(&cfg)
	s := testSession(t, cfg)

	res := callTool(t, s, "weft.script_propose", map[string]any{
		"name": "bad",
		"body": "rm /log/x.log\n",
	})
	if !res.IsError {
		t.Fatalf("expected IsError for forbidden verb")
	}
	if !strings.Contains(textContent(res), "command_forbidden") {
		t.Errorf("expected command_forbidden; got %q", textContent(res))
	}
	if ps, _ := store.List(); len(ps) != 0 {
		t.Errorf("rejected proposal must not persist; got %d staged", len(ps))
	}
}

// TestScriptPropose_DiffAgainstCurrent: when the backend returns
// existing contents for the named script, the diff is a real
// modification — old lines and new lines side by side.
func TestScriptPropose_DiffAgainstCurrent(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	be := &proposeBackend{scripts: map[string]string{
		"err-rate": "cat $1 | grep ERROR\n",
	}}
	be.wire(&cfg)
	s := testSession(t, cfg)

	res := callTool(t, s, "weft.script_propose", map[string]any{
		"name": "err-rate",
		"body": "cat $1 | jq -r .level | uniq -c\n",
	})
	if res.IsError {
		t.Fatalf("unexpected IsError; %q", textContent(res))
	}
	got := textContent(res)
	if !strings.Contains(got, "-cat $1 | grep ERROR") {
		t.Errorf("expected removed line in diff; got %q", got)
	}
	if !strings.Contains(got, "+cat $1 | jq -r .level | uniq -c") {
		t.Errorf("expected added line in diff; got %q", got)
	}
}

// TestCronPropose_HappyPath: cron proposals validate the pipeline
// via DefaultReadPolicy and persist the schedule alongside.
func TestCronPropose_HappyPath(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{}).wire(&cfg)
	s := testSession(t, cfg)

	res := callTool(t, s, "weft.cron_propose", map[string]any{
		"name":     "nightly-error-summary",
		"schedule": "0 0 * * *",
		"pipeline": "cat /log/billing/billing.log | grep ERROR | wc -l",
	})
	if res.IsError {
		t.Fatalf("unexpected IsError; %q", textContent(res))
	}
	ps, _ := store.List()
	if len(ps) != 1 {
		t.Fatalf("expected 1 staged proposal; got %d", len(ps))
	}
	if ps[0].Kind != "cron" {
		t.Errorf("kind = %q, want cron", ps[0].Kind)
	}
	if !strings.Contains(ps[0].Proposed, "0 0 * * *") {
		t.Errorf("proposed body should embed schedule; got %q", ps[0].Proposed)
	}
	if !strings.Contains(ps[0].Proposed, "grep ERROR") {
		t.Errorf("proposed body should embed pipeline; got %q", ps[0].Proposed)
	}
}

// TestCronPropose_PipelineRejected: a forbidden verb in the cron
// pipeline blocks the proposal from being staged.
func TestCronPropose_PipelineRejected(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{}).wire(&cfg)
	s := testSession(t, cfg)
	res := callTool(t, s, "weft.cron_propose", map[string]any{
		"name":     "bad",
		"schedule": "0 0 * * *",
		"pipeline": "rm -rf /log",
	})
	if !res.IsError {
		t.Fatalf("expected IsError")
	}
	if ps, _ := store.List(); len(ps) != 0 {
		t.Errorf("expected nothing staged; got %d", len(ps))
	}
}

// TestCronPropose_BadSchedule: a malformed cron expression fails
// validation. Loose check here — we just want one obvious form
// (5 fields required) to fail; precise cron-syntax tests live
// elsewhere.
func TestCronPropose_BadSchedule(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{}).wire(&cfg)
	s := testSession(t, cfg)
	res := callTool(t, s, "weft.cron_propose", map[string]any{
		"name":     "bad",
		"schedule": "not a cron",
		"pipeline": "cat /log/x/x.log",
	})
	if !res.IsError {
		t.Fatalf("expected IsError")
	}
}

// TestCronPropose_AlertingPipeline: a cron whose pipeline ends in
// `| alert ...` validates through both DefaultCronPolicy (alert
// is in the allowlist) and CompileWatchPipeline (single alert,
// well-formed args).
func TestCronPropose_AlertingPipeline(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{}).wire(&cfg)
	s := testSession(t, cfg)
	res := callTool(t, s, "weft.cron_propose", map[string]any{
		"name":     "billing-error-rate",
		"schedule": "*/5 * * * *",
		"pipeline": "cat /log/billing/billing.log | grep ERROR | wc -l | alert above 50",
	})
	if res.IsError {
		t.Fatalf("unexpected IsError; %q", textContent(res))
	}
	ps, _ := store.List()
	if len(ps) != 1 || ps[0].Kind != "cron" {
		t.Fatalf("expected 1 cron proposal, got %+v", ps)
	}
}

// TestCronPropose_UserDefinedScriptAccepted: a bin/ script previously
// staged through script_propose can be invoked by name in a cron
// pipeline. Without UserDefinedCommands plumbing this fails with
// command_forbidden, defeating the point of saving reusable scripts.
func TestCronPropose_UserDefinedScriptAccepted(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{
		scripts: map[string]string{
			"runtime-field": "tail -n 1 /log/weft/$1/runtime.log | jq -r \".$2\"",
		},
	}).wire(&cfg)
	s := testSession(t, cfg)

	res := callTool(t, s, "weft.cron_propose", map[string]any{
		"name":     "coord-goroutines",
		"schedule": "* * * * *",
		"pipeline": "runtime-field coordinator goroutines",
	})
	if res.IsError {
		t.Fatalf("user-defined script rejected: %q", textContent(res))
	}
}

// TestCronPropose_UnsavedScriptRejected: a script name the customer
// has never staged is still command_forbidden — the policy doesn't
// blanket-trust unknown commands just because they look script-like.
func TestCronPropose_UnsavedScriptRejected(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{}).wire(&cfg)
	s := testSession(t, cfg)

	res := callTool(t, s, "weft.cron_propose", map[string]any{
		"name":     "uses-undefined",
		"schedule": "* * * * *",
		"pipeline": "definitely-not-a-real-script foo",
	})
	if !res.IsError {
		t.Fatal("expected rejection of pipeline referencing undefined script")
	}
	if !strings.Contains(textContent(res), "command_forbidden") {
		t.Errorf("expected command_forbidden detail, got %q", textContent(res))
	}
}

// TestCronPropose_TwoAlertsRejected: even though the verb-allowlist
// admits `alert`, CompileWatchPipeline catches the case where the
// model writes two alert stages — that's a misuse the underlying
// scheduler can't run.
func TestCronPropose_TwoAlertsRejected(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{}).wire(&cfg)
	s := testSession(t, cfg)
	res := callTool(t, s, "weft.cron_propose", map[string]any{
		"name":     "double-alert",
		"schedule": "*/5 * * * *",
		"pipeline": "cat /log/x/x.log | alert above 1 | alert below 0",
	})
	if !res.IsError {
		t.Fatalf("expected IsError for two alerts in one pipeline")
	}
}

// TestProposalDelete_StagesRemoval: a delete proposal records the
// current contents so the human can see what's going away in the
// review.
func TestProposalDelete_StagesRemoval(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	be := &proposeBackend{crons: map[string]string{
		"old-cron": "alert above 50\n",
	}}
	be.wire(&cfg)
	s := testSession(t, cfg)

	res := callTool(t, s, "weft.proposal_delete", map[string]any{
		"kind": "cron",
		"name": "old-cron",
	})
	if res.IsError {
		t.Fatalf("unexpected IsError; %q", textContent(res))
	}
	ps, _ := store.List()
	if len(ps) != 1 {
		t.Fatalf("expected 1 staged proposal; got %d", len(ps))
	}
	if ps[0].Kind != "delete" || ps[0].SubKind != "cron" {
		t.Errorf("expected delete/cron; got %+v", ps[0])
	}
	if !strings.Contains(ps[0].Diff, "/dev/null") {
		t.Errorf("delete diff should mention /dev/null; got:\n%s", ps[0].Diff)
	}
}

// TestProposalDelete_MissingResource: deleting something that the
// backend doesn't know about returns IsError so the model doesn't
// flood the proposal list with no-op deletes.
func TestProposalDelete_MissingResource(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{}).wire(&cfg)
	s := testSession(t, cfg)
	res := callTool(t, s, "weft.proposal_delete", map[string]any{
		"kind": "cron",
		"name": "does-not-exist",
	})
	if !res.IsError {
		t.Fatalf("expected IsError when resource doesn't exist")
	}
}

// TestDashboardPropose_HappyPath: a valid dashboard YAML stages
// cleanly when every referenced watch exists.
func TestDashboardPropose_HappyPath(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{watches: []string{"err-rate", "pending"}}).wire(&cfg)
	s := testSession(t, cfg)

	body := `title: Production
sections:
  - title: Errors
    tiles:
      - kind: sparkline
        cron: err-rate
        size: md
      - kind: counter
        cron: pending
`
	res := callTool(t, s, "weft.dashboard_propose", map[string]any{
		"name": "prod",
		"body": body,
	})
	if res.IsError {
		t.Fatalf("unexpected IsError; %q", textContent(res))
	}
	ps, _ := store.List()
	if len(ps) != 1 {
		t.Fatalf("expected 1 staged proposal; got %d", len(ps))
	}
	if ps[0].Kind != "dashboard" || ps[0].Name != "prod" {
		t.Errorf("staged proposal mismatch: %+v", ps[0])
	}
	if !strings.Contains(ps[0].Proposed, "title: Production") {
		t.Errorf("proposed body lost title; got %q", ps[0].Proposed)
	}
}

// TestDashboardPropose_BadYAMLRejected: ParseDashboardYAML errors
// surface as IsError without staging.
func TestDashboardPropose_BadYAMLRejected(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{}).wire(&cfg)
	s := testSession(t, cfg)

	res := callTool(t, s, "weft.dashboard_propose", map[string]any{
		"name": "broken",
		"body": "tiles:\n  - kind: pizza\n    cron: foo\n",
	})
	if !res.IsError {
		t.Fatalf("expected IsError for unknown kind")
	}
	if !strings.Contains(textContent(res), "unknown kind") {
		t.Errorf("expected unknown-kind message; got %q", textContent(res))
	}
	if ps, _ := store.List(); len(ps) != 0 {
		t.Errorf("rejected proposal must not persist; got %d staged", len(ps))
	}
}

// TestDashboardPropose_UnknownWatchRejected: every tile.watch is
// validated against ListWatches; unknown names abort the propose.
func TestDashboardPropose_UnknownWatchRejected(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{watches: []string{"err-rate"}}).wire(&cfg)
	s := testSession(t, cfg)

	body := `tiles:
  - kind: counter
    cron: ghost
`
	res := callTool(t, s, "weft.dashboard_propose", map[string]any{
		"name": "broken",
		"body": body,
	})
	if !res.IsError {
		t.Fatalf("expected IsError for unknown watch")
	}
	if !strings.Contains(textContent(res), "ghost") {
		t.Errorf("expected unknown watch named in error; got %q", textContent(res))
	}
}

// TestDashboardPropose_DiffAgainstCurrent: when an existing
// dashboard is replaced, the diff shows real removals + additions.
func TestDashboardPropose_DiffAgainstCurrent(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{
		watches: []string{"a", "b"},
		dashboards: map[string]string{
			"prod": "tiles:\n  - kind: counter\n    cron: a\n",
		},
	}).wire(&cfg)
	s := testSession(t, cfg)

	res := callTool(t, s, "weft.dashboard_propose", map[string]any{
		"name": "prod",
		"body": "tiles:\n  - kind: counter\n    cron: b\n",
	})
	if res.IsError {
		t.Fatalf("unexpected IsError; %q", textContent(res))
	}
	got := textContent(res)
	if !strings.Contains(got, "-    cron: a") {
		t.Errorf("expected removed line in diff; got %q", got)
	}
	if !strings.Contains(got, "+    cron: b") {
		t.Errorf("expected added line in diff; got %q", got)
	}
}

// TestProposalDelete_Dashboard: dashboard kind is recognized by
// proposal_delete and the current contents are recorded.
func TestProposalDelete_Dashboard(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{
		dashboards: map[string]string{"old": "title: Old\ntiles:\n  - kind: counter\n    cron: x\n"},
	}).wire(&cfg)
	s := testSession(t, cfg)

	res := callTool(t, s, "weft.proposal_delete", map[string]any{
		"kind": "dashboard",
		"name": "old",
	})
	if res.IsError {
		t.Fatalf("unexpected IsError; %q", textContent(res))
	}
	ps, _ := store.List()
	if len(ps) != 1 || ps[0].SubKind != "dashboard" {
		t.Errorf("expected 1 dashboard delete proposal; got %+v", ps)
	}
}

func TestCronPropose_TimeOfDayScheduleRejected(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{}).wire(&cfg)
	s := testSession(t, cfg)
	res := callTool(t, s, "weft.cron_propose", map[string]any{
		"name":     "morning",
		"schedule": "0 9 * * *",
		"pipeline": "cat /log/myco/prod/api/main.log | wc -l",
	})
	if !res.IsError {
		t.Fatalf("expected IsError for a time-of-day schedule")
	}
	if ps, _ := store.List(); len(ps) != 0 {
		t.Errorf("expected nothing staged; got %d", len(ps))
	}
}

// The composite recipe from the alerts manual can be proposed.
func TestCronPropose_CompositeAlert(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	(&proposeBackend{}).wire(&cfg)
	s := testSession(t, cfg)

	res := callTool(t, s, "weft.cron_propose", map[string]any{
		"name":     "deploy-danger",
		"schedule": "every 1m",
		"pipeline": "(weft::is_above error-rate 50 && weft::is_absent canary-ping && echo fire) | alert --notify=critical",
	})
	if res.IsError {
		t.Fatalf("composite alert rejected: %q", textContent(res))
	}
}
