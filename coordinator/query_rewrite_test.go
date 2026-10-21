package coordinator

import (
	"context"
	"testing"

	"github.com/graybeardcorp/warp/pkg/bash"
)

// TestRewriteQueryPipelines_PreservesStdoutRedirect pins that when
// rewriteQueryPipelines substitutes a log pipeline with a
// CallbackCommand, any `>` redirect on the displaced pipeline is
// carried onto the callback. Without this, a /bin script body like
// `cat /log/foo.log > /tmp/snapshot` silently produces nothing —
// warp's rewriter swaps in a callback whose stdout flows through to
// the parent context, while the redirect that should have caught it
// is dropped on the floor.
func TestRewriteQueryPipelines_PreservesStdoutRedirect(t *testing.T) {
	script, err := bash.Parse(`cat /log/foo.log > /tmp/snapshot`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// rewriteQueryPipelines is the production rewriter the coordinator
	// installs on warp interpreters. It walks the AST and substitutes
	// log-shaped pipelines with CallbackCommands. Pass nil for the
	// coordinator-bound parameters — we only inspect the resulting AST
	// shape here, the callback's body is irrelevant to this test.
	rewriteQueryPipelines(context.Background(), script, nil, TimeRange{}, "kevlar", 0, "", nil)

	cb := singleCallbackInScript(t, script)
	if got := len(cb.Redirects); got != 1 {
		t.Fatalf("expected callback to carry 1 redirect (the displaced `> /tmp/snapshot`), got %d", got)
	}
	r := cb.Redirects[0]
	if r.Op != ">" {
		t.Errorf("redirect op = %q, want %q", r.Op, ">")
	}
	target := r.Target.Literal()
	if target != "/tmp/snapshot" {
		t.Errorf("redirect target = %q, want %q", target, "/tmp/snapshot")
	}
}

// TestRewriteQueryPipelines_PreservesAppendRedirect — same, for `>>`.
// The append flag must round-trip; otherwise a script that incremen-
// tally appends query results to a tmp file would clobber it instead.
func TestRewriteQueryPipelines_PreservesAppendRedirect(t *testing.T) {
	script, err := bash.Parse(`tail -n 100 /log/api.log >> /tmp/audit`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rewriteQueryPipelines(context.Background(), script, nil, TimeRange{}, "kevlar", 0, "", nil)

	cb := singleCallbackInScript(t, script)
	if got := len(cb.Redirects); got != 1 {
		t.Fatalf("expected 1 redirect, got %d", got)
	}
	if cb.Redirects[0].Op != ">>" {
		t.Errorf("op = %q, want %q", cb.Redirects[0].Op, ">>")
	}
}

// TestRewriteQueryPipelines_RedirectOnLastStage — for a multi-stage
// pipeline `cat /log/foo | grep ERROR > out`, bash semantics put the
// redirect on grep (the last stage), but the entire pipeline becomes
// one callback. The redirect must be lifted from the last command and
// attached to the callback so the captured pipeline output lands at
// `out`.
func TestRewriteQueryPipelines_RedirectOnLastStage(t *testing.T) {
	script, err := bash.Parse(`cat /log/foo.log | grep ERROR > /tmp/errors`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rewriteQueryPipelines(context.Background(), script, nil, TimeRange{}, "kevlar", 0, "", nil)

	cb := singleCallbackInScript(t, script)
	if got := len(cb.Redirects); got != 1 {
		t.Fatalf("expected 1 redirect lifted from last stage, got %d", got)
	}
	if cb.Redirects[0].Op != ">" {
		t.Errorf("op = %q, want %q", cb.Redirects[0].Op, ">")
	}
	if got := cb.Redirects[0].Target.Literal(); got != "/tmp/errors" {
		t.Errorf("target = %q, want %q", got, "/tmp/errors")
	}
}

// TestRewriteQueryPipelines_NoRedirect — sanity: a plain query
// pipeline produces a callback with no redirects. Guards against an
// over-eager harvester that allocates a slice for nothing.
func TestRewriteQueryPipelines_NoRedirect(t *testing.T) {
	script, err := bash.Parse(`cat /log/foo.log`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rewriteQueryPipelines(context.Background(), script, nil, TimeRange{}, "kevlar", 0, "", nil)

	cb := singleCallbackInScript(t, script)
	if len(cb.Redirects) != 0 {
		t.Errorf("expected no redirects on bare cat callback, got %d: %+v", len(cb.Redirects), cb.Redirects)
	}
}

// TestRewriteQueryPipelines_LeadingNonSourceCommandWithLogPath pins the
// fix for the "pipeline whose first stage isn't cat/head/tail silently
// returns nothing" gap (docs/known-issues.md): `grep ERROR /log/foo.log`
// — grep given the path directly, as real Unix grep would accept,
// instead of being piped a stream — must still get substituted into the
// query callback so planner.Plan rejects it with a clear error. Before
// the fix, isQueryShape returned false for any leading command other
// than cat/head/tail, so this pipeline was left untouched and fell
// through to plain warp execution against shellfs's LogMount, which
// silently returned zero rows with no error at all.
func TestRewriteQueryPipelines_LeadingNonSourceCommandWithLogPath(t *testing.T) {
	script, err := bash.Parse(`grep ERROR /log/weft/coordinator/main.log`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rewriteQueryPipelines(context.Background(), script, nil, TimeRange{}, "kevlar", 0, "", nil)

	// singleCallbackInScript fatals if the rewrite didn't substitute a
	// callback at all — exactly the pre-fix behavior for this shape.
	singleCallbackInScript(t, script)
}

// TestRewriteQueryPipelines_TraceRecognized pins that `trace --id <id>` is
// recognized as a query shape even though its argument isn't a
// filesystem path — isQueryShape's "trace" case must stay in sync with
// planner/parse.go's parseSource "trace" case.
func TestRewriteQueryPipelines_TraceRecognized(t *testing.T) {
	script, err := bash.Parse(`trace --id abc-123`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rewriteQueryPipelines(context.Background(), script, nil, TimeRange{}, "kevlar", 0, "", nil)

	singleCallbackInScript(t, script)
}

// singleCallbackInScript walks the rewritten script, asserts that
// exactly one CallbackCommand was substituted (which is what the
// rewriter does for a single-pipeline log query), and returns it.
// Reused across the rewriter tests above.
func singleCallbackInScript(t *testing.T, script *bash.Script) *bash.CallbackCommand {
	t.Helper()
	var found []*bash.CallbackCommand
	bash.Walk(script, func(n bash.Node) bool {
		if cb, ok := n.(*bash.CallbackCommand); ok {
			found = append(found, cb)
		}
		return true
	})
	if len(found) != 1 {
		t.Fatalf("expected 1 CallbackCommand after rewrite, got %d", len(found))
	}
	return found[0]
}

// TestRewriteQueryPipelines_FilesystemCommandWithLogPath: cd/ls/tree on a
// /log/ directory are filesystem navigation, not queries. They must be
// left untouched for plain warp+shellfs to serve — wrapping them in a
// query callback made `ls /log/loom` and `cd /log/loom/prod/api` fail
// with "pipeline must start with cat, head, or tail".
func TestRewriteQueryPipelines_FilesystemCommandWithLogPath(t *testing.T) {
	for _, src := range []string{
		`ls /log/loom`,
		`ls -l /log/loom/prod/api`,
		`cd /log/loom/prod/api`,
		`tree /log/loom`,
		`ls /log/loom | grep prod`,
	} {
		script, err := bash.Parse(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		rewriteQueryPipelines(context.Background(), script, nil, TimeRange{}, "kevlar", 0, "", nil)
		n := 0
		bash.Walk(script, func(node bash.Node) bool {
			if _, ok := node.(*bash.CallbackCommand); ok {
				n++
			}
			return true
		})
		if n != 0 {
			t.Errorf("%q: got %d query callbacks, want 0 (filesystem command must not be wrapped)", src, n)
		}
	}
}
