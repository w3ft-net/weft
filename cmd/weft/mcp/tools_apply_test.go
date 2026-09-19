package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/cmd/weft/mcp/proposals"
	"github.com/graybeardcorp/weft/store"
)

// applyBackend captures every Save / Delete call so tests can
// verify that proposals_apply landed the expected resources.
type applyBackend struct {
	saved   map[string]string // key="kind:name" → body
	deleted map[string]bool   // key="kind:name"
}

func newApplyBackend() *applyBackend {
	return &applyBackend{saved: map[string]string{}, deleted: map[string]bool{}}
}

func (b *applyBackend) wire(cfg *Config) {
	cfg.SaveScript = func(name, body string) error { b.saved["script:"+name] = body; return nil }
	cfg.SaveCron = func(name, body string) error { b.saved["cron:"+name] = body; return nil }
	cfg.DeleteScript = func(name string) error { b.deleted["script:"+name] = true; return nil }
	cfg.DeleteCron = func(name string) error { b.deleted["cron:"+name] = true; return nil }
}

// TestProposalsList: list returns every staged proposal with id +
// kind + name in a structured table the model can show the human
// before deciding what to apply.
func TestProposalsList(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	newApplyBackend().wire(&cfg)
	s := testSession(t, cfg)

	// Stage two proposals via the underlying store so this test
	// stays focused on List behaviour.
	_, _ = store.Add(proposals.Proposal{Kind: "script", Name: "a", Proposed: "cat $1\n"})
	_, _ = store.Add(proposals.Proposal{Kind: "cron", Name: "b", Proposed: "schedule: */5 * * * *\npipeline: cat /log/x.log | alert above 1\n"})

	res := callTool(t, s, "weft.proposals_list", map[string]any{})
	if res.IsError {
		t.Fatalf("unexpected IsError; %q", textContent(res))
	}
	got := textContent(res)
	for _, want := range []string{"0001", "0002", "script", "cron", "a", "b"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in list output; got %q", want, got)
		}
	}
}

// TestProposalsDiff: diff returns the unified-diff text for one
// proposal, looked up by id. The model uses this when it wants to
// re-show a specific change at apply time.
func TestProposalsDiff(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	newApplyBackend().wire(&cfg)
	s := testSession(t, cfg)
	_, _ = store.Add(proposals.Proposal{Kind: "script", Name: "x", Proposed: "cat $1\n"})

	res := callTool(t, s, "weft.proposals_diff", map[string]any{"id": "0001"})
	if res.IsError {
		t.Fatalf("unexpected IsError; %q", textContent(res))
	}
	if !strings.Contains(textContent(res), "+cat $1") {
		t.Errorf("expected unified diff; got %q", textContent(res))
	}
}

// TestProposalsDiff_MissingId: a bogus id returns IsError so the
// model gets a clean signal rather than empty text.
func TestProposalsDiff_MissingId(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	newApplyBackend().wire(&cfg)
	s := testSession(t, cfg)
	res := callTool(t, s, "weft.proposals_diff", map[string]any{"id": "9999"})
	if !res.IsError {
		t.Fatalf("expected IsError")
	}
}

// TestProposalsApply_AppliesAllAndRemoves: the success path —
// every requested proposal lands at the matching backend method
// and is then removed from the store.
func TestProposalsApply_AppliesAllAndRemoves(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	be := newApplyBackend()
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	be.wire(&cfg)
	s := testSession(t, cfg)

	_, _ = store.Add(proposals.Proposal{Kind: "script", Name: "a", Proposed: "cat $1\n"})
	_, _ = store.Add(proposals.Proposal{Kind: "cron", Name: "b", Proposed: "schedule: */5 * * * *\npipeline: cat /log/x.log | alert above 1\n"})
	_, _ = store.Add(proposals.Proposal{Kind: "delete", SubKind: "script", Name: "old"})

	res := callTool(t, s, "weft.proposals_apply", map[string]any{
		"ids": []string{"0001", "0002", "0003"},
	})
	if res.IsError {
		t.Fatalf("unexpected IsError; %q", textContent(res))
	}
	if got := be.saved["script:a"]; got != "cat $1\n" {
		t.Errorf("SaveScript a body = %q", got)
	}
	if got := be.saved["cron:b"]; !strings.Contains(got, "alert above 1") {
		t.Errorf("SaveCron b body = %q", got)
	}
	if !be.deleted["script:old"] {
		t.Errorf("DeleteScript old not called; deleted=%v", be.deleted)
	}
	if ps, _ := store.List(); len(ps) != 0 {
		t.Errorf("expected store cleared; got %d remaining", len(ps))
	}
}

// TestProposalsApply_PartialFailure_KeepsRest: when one save fails
// mid-batch, the failed proposal stays in the store and remaining
// IDs in the request are skipped — the human should see clearly
// which landed and which did not, then re-review.
func TestProposalsApply_PartialFailure_KeepsRest(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	be := newApplyBackend()
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	be.wire(&cfg)
	cfg.SaveCron = func(name, body string) error {
		return fmt.Errorf("backend down")
	}
	s := testSession(t, cfg)

	_, _ = store.Add(proposals.Proposal{Kind: "script", Name: "a", Proposed: "cat $1\n"})
	_, _ = store.Add(proposals.Proposal{Kind: "cron", Name: "b", Proposed: "schedule: */5 * * * *\npipeline: cat /log/x.log | alert above 1\n"})
	_, _ = store.Add(proposals.Proposal{Kind: "script", Name: "c", Proposed: "cat $2\n"})

	res := callTool(t, s, "weft.proposals_apply", map[string]any{
		"ids": []string{"0001", "0002", "0003"},
	})
	if !res.IsError {
		t.Fatalf("expected IsError on partial failure")
	}

	if _, ok := be.saved["script:a"]; !ok {
		t.Errorf("SaveScript a should have been called before failure")
	}
	if _, ok := be.saved["script:c"]; ok {
		t.Errorf("SaveScript c should not have been called after a failure")
	}
	// 0001 was applied → removed; 0002 failed → still staged;
	// 0003 was never attempted → still staged.
	ps, _ := store.List()
	stillStaged := map[string]bool{}
	for _, p := range ps {
		stillStaged[p.ID] = true
	}
	if stillStaged["0001"] {
		t.Errorf("0001 should be removed after success")
	}
	if !stillStaged["0002"] || !stillStaged["0003"] {
		t.Errorf("0002 and 0003 should remain staged; got %+v", stillStaged)
	}
}

// TestProposalsApply_WarningDoesNotBlockBatch: a SaveCron that
// returns a non-blocking store.Warning (e.g. the /cron.d truncation
// lint, surfaced end-to-end through SaveWatch → the HTTP response's
// "warnings" field) must still count as applied — not as a failure —
// with the message folded into the result text. Generic by
// construction: this doesn't special-case what produced the warning.
func TestProposalsApply_WarningDoesNotBlockBatch(t *testing.T) {
	store_, _ := proposals.New(t.TempDir())
	be := newApplyBackend()
	cfg := Config{Run: neverRunFn(t), Proposals: store_}
	be.wire(&cfg)
	cfg.SaveCron = func(name, body string) error {
		be.saved["cron:"+name] = body
		return store.NewWarning("tail -n 1000 caps this cron at 1000 rows per run")
	}
	s := testSession(t, cfg)

	_, _ = store_.Add(proposals.Proposal{Kind: "cron", Name: "risky", Proposed: "schedule: */5 * * * *\npipeline: tail -n 1000 x | grep -c y\n"})

	res := callTool(t, s, "weft.proposals_apply", map[string]any{"ids": []string{"0001"}})
	if res.IsError {
		t.Fatalf("unexpected IsError for a warning-only save; %q", textContent(res))
	}
	if _, ok := be.saved["cron:risky"]; !ok {
		t.Errorf("SaveCron risky should have been called")
	}
	if ps, _ := store_.List(); len(ps) != 0 {
		t.Errorf("applied proposal should be removed from staging; got %d remaining", len(ps))
	}
	got := textContent(res)
	if !strings.Contains(got, "applied 1 proposal(s)") {
		t.Errorf("result should report the successful apply; got %q", got)
	}
	if !strings.Contains(got, "0001") || !strings.Contains(got, "tail -n 1000 caps this cron") {
		t.Errorf("result should fold in the warning text keyed by proposal id; got %q", got)
	}
}

// TestProposalsApply_UnknownId: an id not in the store fails the
// whole batch immediately, before any backend mutation. We don't
// want to half-apply because of a typo.
func TestProposalsApply_UnknownId(t *testing.T) {
	store, _ := proposals.New(t.TempDir())
	be := newApplyBackend()
	cfg := Config{Run: neverRunFn(t), Proposals: store}
	be.wire(&cfg)
	s := testSession(t, cfg)
	_, _ = store.Add(proposals.Proposal{Kind: "script", Name: "a", Proposed: "cat $1\n"})

	res := callTool(t, s, "weft.proposals_apply", map[string]any{
		"ids": []string{"0001", "9999"},
	})
	if !res.IsError {
		t.Fatalf("expected IsError")
	}
	if len(be.saved) != 0 {
		t.Errorf("nothing should have been saved; got %+v", be.saved)
	}
	ps, _ := store.List()
	if len(ps) != 1 {
		t.Errorf("expected 1 still staged; got %d", len(ps))
	}
}
