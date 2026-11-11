package coordinator

import (
	"bytes"
	"io"
	"strconv"
	"strings"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/warp/pkg/jq"
	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
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
type LiveFilter func(Row) []string

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
	gate := &RowMatcher{Coords: plan.Coords}

	// Leading row stages keep or drop the live row (LiveRowStages), and
	// a jq after them reads its payload (LiveReadsPayload). Same rule
	// the compute engine applies to the batch; see planner.QueryPlan.
	var rowStages *compute.RowStages
	if len(plan.LiveRowStages) > 0 {
		rs, err := compute.NewRowStages(plan.LiveRowStages)
		if err != nil {
			// Plan only emits row stages it parsed; see the parse
			// error note below.
			return func(Row) []string { return nil }
		}
		rowStages = rs
	}
	admit := func(row Row) bool {
		if !gate.Match(row) {
			return false
		}
		if rowStages == nil {
			return true
		}
		var meta map[string]string
		if rowStages.ReadsMeta() {
			meta = liveMetaVars(row.LogRow)
		}
		return rowStages.Keep(row.Raw, string(row.PayloadJson), meta)
	}
	// The meta-variables a first jq stage reads are bound for its input.
	var jqVars *compute.JqBinder
	if len(plan.LiveStages) > 0 && strings.HasPrefix(plan.LiveStages[0], "jq ") {
		if read := planner.StageMetaRead(plan.LiveStages[0]); read.Reads() {
			jqVars = compute.NewJqBinder(read)
		}
	}
	input := func(row *pb.LogRow) string {
		if plan.LiveReadsPayload {
			payload := string(row.PayloadJson)
			if payload == "" {
				payload = "{}"
			}
			return payload
		}
		return row.Raw
	}

	passthrough := func(row Row) []string {
		if !admit(row) {
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

	return func(row Row) []string {
		if !admit(row) {
			return nil
		}
		var vars map[string]jq.Value
		if jqVars != nil {
			vars = jqVars.Vars(liveMetaVars(row.LogRow))
		}
		return runStages(input(row.LogRow), ast, vars)
	}
}

// liveMetaVars is a live row's meta-variable columns, ts included, in
// the shape compute builds them for a batch row, so jq sees the same
// meta-variables live as in the batch.
func liveMetaVars(row *pb.LogRow) map[string]string {
	meta := map[string]string{
		"namespace":     row.Namespace,
		"service":       row.Service,
		"kind":          row.Kind,
		"level":         row.Level,
		"instance":      row.Instance,
		"deployment":    row.Deployment,
		"env":           row.Env,
		"version":       row.Version,
		"region":        row.Region,
		"cluster":       row.Cluster,
		"k8s_namespace": row.K8SNamespace,
		"pod":           row.Pod,
		"container":     row.Container,
		"node":          row.Node,
		"trace_id":      row.TraceId,
		"span_name":     row.SpanName,
		"span_id":       row.SpanId,
		"duration_ns":   strconv.FormatInt(row.DurationNs, 10),
		"ts":            compute.FormatTS(row.TsUnixNs),
	}
	meta["event_id"] = row.EventId
	meta["line_no"] = strconv.Itoa(int(row.LineNo))
	return meta
}

// runStages feeds raw to a fresh bash interpreter running the parsed
// pipeline and returns the stdout lines. A new interpreter per call
// keeps awk/sed/jq state isolated from prior rows — future work can
// trade that for a long-lived per-subscription interpreter if the
// per-row cost bites.
func runStages(raw string, ast *bash.Script, jqVars map[string]jq.Value) []string {
	interp := bash.NewInterpreter()
	interp.JqCompact = true // as in compute; see compute/engine.go
	if jqVars != nil {
		interp.JqInputVars = func() map[string]jq.Value { return jqVars }
	}
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
