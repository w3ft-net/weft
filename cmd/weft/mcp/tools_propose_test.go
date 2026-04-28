package mcp

import (
	"strings"
	"testing"

	"github.com/kevlarcon/weft/cmd/weft/mcp/proposals"
)

// proposeBackend is the stub used across propose-tool tests.
// Each map represents one of the resource namespaces; the tool
// looks up "current" contents via the matching Get function.
type proposeBackend struct {
	scripts map[string]string
	crons   map[string]string
}

func (b *proposeBackend) wire(cfg *Config) {
	cfg.GetScript = func(name string) (string, error) { return b.scripts[name], nil }
	cfg.GetCron = func(name string) (string, error) { return b.crons[name], nil }
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
		"schedule": "0 2 * * *",
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
	if !strings.Contains(ps[0].Proposed, "0 2 * * *") {
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
		"schedule": "0 2 * * *",
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
