package coordinator

import (
	"bytes"
	"io"
	"strings"

	"github.com/kevlarcon/warp/pkg/bash"
	"github.com/kevlarcon/weft/planner"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// LiveFilter applies a follow-mode pipeline's per-row stages to a single
// ingested row and returns zero or more output lines. Empty return means
// the row was filtered out of the live stream; a non-empty return is
// the text the subscriber should see.
//
// The filter mirrors on a live row whatever the initial-tail batch ran
// through the compute pool. For the initial pass the planner hands
// compute an optimized program (pushdown, projection absorption); for
// the live pass we re-run the user-written stages per row via warp's
// bash interpreter, so the two agree on what the pipeline means.
type LiveFilter func(*pb.LogRow) []string

// BuildLiveFilter constructs a per-row closure from a parsed QueryPlan.
// Callers own one per subscription.
//
// The source gate runs first and short-circuits non-matching rows
// without touching the interpreter — a coordinate-only RowMatcher
// (namespace/service/kind, no grep patterns) mirrors the SQL source
// predicate. Rows passing the gate fall through to the per-row
// pipeline — or, when the plan has no stages (bare `tail -f foo.log`),
// straight to the caller as the raw line.
func BuildLiveFilter(plan *planner.QueryPlan) LiveFilter {
	gate := &RowMatcher{
		Namespace: plan.SourceNamespace,
		Service:   plan.SourceService,
		Kind:      plan.SourceKind,
	}

	passthrough := func(row *pb.LogRow) []string {
		if !gate.Match(row) {
			return nil
		}
		return []string{row.Raw}
	}

	if len(plan.LiveStages) == 0 {
		return passthrough
	}

	// Parse once at build time — every row shares the AST; the
	// interpreter is fresh per row so there's no shared state to leak
	// across invocations. A parse error here is a planner bug (we
	// reconstructed an invalid pipeline from accepted input); fall
	// back to passthrough rather than swallow the bug silently — the
	// caller still sees rows while we debug.
	script := strings.Join(plan.LiveStages, " | ")
	ast, err := bash.Parse(script)
	if err != nil {
		return passthrough
	}

	return func(row *pb.LogRow) []string {
		if !gate.Match(row) {
			return nil
		}
		return runStages(row.Raw, ast)
	}
}

// runStages feeds raw to a fresh bash interpreter running the parsed
// pipeline and returns the stdout lines. A new interpreter per call
// keeps awk/sed/jq state isolated from prior rows — future work can
// trade that for a long-lived per-subscription interpreter if the
// per-row cost bites.
func runStages(raw string, ast *bash.Script) []string {
	interp := bash.NewInterpreter()
	bash.RegisterToolCommands(interp)
	interp.SetStdin(strings.NewReader(raw + "\n"))
	var out bytes.Buffer
	interp.SetStdout(&out)
	interp.SetStderr(io.Discard)
	interp.ExecScript(ast)

	text := out.String()
	if text == "" {
		return nil
	}
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}
