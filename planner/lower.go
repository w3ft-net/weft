package planner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"strconv"
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
)

// PlannerVersion stamps every LoweredPlan with the version of the
// planner that produced it. Bump this on any change to Plan() that
// would alter the SQL shape for a given pipeline (subquery wrap,
// merge-key flip, predicate pushdown rule). A version mismatch at
// fire time forces a recompile.
//
// v2: dropped the composite follow_app/source_app fields; the
// (namespace, service, kind) columns are carried split.
// v3: the row-level-security predicate is `customer_id = CAST(? AS BIGINT)`
// and LoweredPlan carries customer_id (was the customer name).
// v4: compute stages are the user's stages verbatim (no synthesized awk/jq
// remainders); awk $N and cut -f no longer push down; a jq stage reads
// the payload; row stages run per row (RowStages).
// v5: a projection stays in compute when the stage after it names a
// meta-variable (folding it would hand that stage the row's instead).
// v6: awk's $N lowers to fields[N] (ingest splits fields like awk), and a
// comparison with a number, or a numeric-looking string, is a prefilter.
// v7: wc -l, sort -u and sort | uniq [-c] aggregate in the shard
// (Aggregate); cut and jq -r project.
// v8: grep and sed after a plain sort or the SELECT's projection filter
// each line in the shard; sort with an operand no longer orders or
// aggregates there.
// v9: every row query selects event_id and line_no after (raw, ts), and a
// ts order is (ts, event_id, line_no), one-line events first.
// v10: an awk with an exactly-lowering pattern and a print SQL computes
// (`$3 == "x" { print HOST, $0 }`) is Filter then Project in the shard,
// not a prefilter plus a compute stage.
// v11: the span aggregates group by service and count the rows that carry a
// level (null errors when none do), and a trace's arguments changed.
// v12: the shard-side grouping of wc -l, sort -u and sort | uniq [-c] is a
// Group, not an Aggregate string.
// v13: more Groups: sort | uniq -c | sort -rn [| head -n N] is ordered by
// count and cut to N in compute, sort -u | wc -l is a distinct total, and
// awk's count per key is a key count.
const PlannerVersion = 13

// LoweredPlan is the persisted shape we store next to a watch's
// pipeline string. It captures everything the runtime needs to
// dispatch the query without re-running Plan() — the SQL strings,
// the arg sources (so customer + time bounds can be re-resolved
// against fresh fire context), the compute stages, and the
// metadata that watches/dashboards consume (alert args,
// source coordinates, follow flags). BinDeps records each /bin/ inlined during
// lowering with its content hash so a /bin/ edit invalidates the
// lowered plan on the next fire.
//
// JSON shape is deliberately stable — this lands on a Postgres JSONB
// column and we want to be able to inspect it with `jq` from a
// `weft.cat /cron.d/foo` or a SQL query. New fields go at the end
// with omitempty; renames trigger a PlannerVersion bump.
type LoweredPlan struct {
	// Customer is the row-level-security boundary captured at save
	// time. The fire path uses it to construct ResolveCtx without
	// looking up the customer name by ID — which is the dominant
	// non-warp cost of an uncached cron fire (see Phase A bench
	// profile). Customer renames invalidate this field; the fire
	// path will need to re-lower if it diverges, but renames are
	// rare enough that the bake-in is worth the saved DB hit.
	CustomerID int64 `json:"customer_id"`
	// Tier is the customer's tier at save time. Cached here so the
	// fire path can clamp the time-range retention + select shards
	// (both tier-dependent) without two DB roundtrips per fire. A
	// tier change between save and fire causes the watch to use the
	// stale tier until re-lowering — acceptable since tier changes
	// are rare and a user upgrading their plan can re-save watches
	// to pick up extended retention immediately. The fire path
	// could instead invalidate on tier change via subscription
	// hooks, but that's wider scope than this commit.
	Tier              string        `json:"tier"`
	ShardSQL          string        `json:"shard_sql"`
	DirectShardSQL    string        `json:"direct_shard_sql"`
	ArgSources        []ArgSource   `json:"args,omitempty"`
	ComputeStages     []string      `json:"compute_stages,omitempty"`
	RowStages         []string      `json:"row_stages,omitempty"`
	StagesReadPayload bool          `json:"stages_read_payload,omitempty"`
	LiveStages        []string      `json:"live_stages,omitempty"`
	MergeKey          string        `json:"merge_key,omitempty"`
	Aggregate         string        `json:"aggregate,omitempty"`
	Group             *GroupSpec    `json:"group,omitempty"`
	Limit             int           `json:"limit,omitempty"`
	ShardLimit        int           `json:"shard_limit,omitempty"`
	TailMode          bool          `json:"tail_mode,omitempty"`
	Follow            bool          `json:"follow,omitempty"`
	FollowPatterns    []GrepPattern `json:"follow_patterns,omitempty"`
	SourceCmd         string        `json:"source_cmd,omitempty"`
	SourceNamespace   string        `json:"source_namespace,omitempty"`
	SourceService     string        `json:"source_service,omitempty"`
	SourceKind        string        `json:"source_kind,omitempty"`
	AlertArgs         []string      `json:"alert_args,omitempty"`
	RedirectTarget    string        `json:"redirect_target,omitempty"`
	RedirectAppend    bool          `json:"redirect_append,omitempty"`
	BinDeps           []BinDep      `json:"bin_deps,omitempty"`
	CompiledAt        time.Time     `json:"compiled_at"`
	PlannerVersion    int           `json:"planner_version"`
}

// BinDep is one /bin/ script the lowered pipeline inlines. Hash is
// the SHA-256 of the inlined source — if it differs from the current
// shell_functions row at fire time, the lowered plan is stale and
// must be recompiled.
type BinDep struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
}

// BinResolver lets Lower read /bin/<name> source for inlining without
// taking a dependency on the store package. The implementation lives
// in coordinator or api code and bridges to store.GetShellFunction.
type BinResolver interface {
	// Source returns the script body + a stable content hash. ok=false
	// when the /bin doesn't exist — the caller treats this as "leave
	// the /bin/foo call unexpanded" (matches expandBinPipeline's
	// existing behavior).
	Source(name string) (script, hash string, ok bool)
}

// ResolveCtx supplies the runtime values that fill in the non-Literal
// ArgSources at fire time. Lower runs without one — its output is
// symbolic; Hydrate consumes the lowered form plus a ResolveCtx to
// produce concrete dispatch args.
type ResolveCtx struct {
	CustomerID int64
	Start      time.Time
	End        time.Time
}

// Resolve produces the concrete value for one ArgSource. For
// ArgLiteral the value comes from the lowered form itself; the
// runtime kinds (Customer, TimeStart, TimeEnd) read from ctx.
func (a ArgSource) Resolve(ctx ResolveCtx) string {
	switch a.Kind {
	case ArgLiteral:
		return a.Value
	case ArgCustomer:
		return strconv.FormatInt(ctx.CustomerID, 10)
	case ArgTimeStart:
		return formatTS(ctx.Start)
	case ArgTimeEnd:
		return formatTS(ctx.End)
	}
	return ""
}

// Lower compiles a pipeline at save time. customer fixes the row-
// level-security boundary for the duration of this lowering call;
// the resulting LoweredPlan carries the customer as ArgCustomer
// (re-resolved at each fire from the watch's stored OrgID).
// tier is captured so the fire path can clamp retention + select
// shards without a DB lookup; pass "" if the caller doesn't have
// it (the fire path falls back to looking it up).
//
// resolveBin reads /bin/<name> source for top-level bash inlining.
// Pass a no-op resolver if the caller doesn't need /bin expansion.
//
// Time bounds are not supplied here. Lower passes a sentinel time
// range into Plan() so the ts predicates get emitted with
// ArgTimeStart / ArgTimeEnd sources; Hydrate replaces the Values
// at fire time.
func Lower(pipeline string, customerID int64, tier string, resolveBin BinResolver) (*LoweredPlan, error) {
	expanded, deps, err := expandForLowering(pipeline, resolveBin)
	if err != nil {
		return nil, fmt.Errorf("lower: expand /bin: %w", err)
	}

	// Sentinel time bounds. The values are irrelevant — Hydrate
	// replaces them at fire time. We need NON-zero values so
	// addTimeBounds actually emits the predicates (zero is the
	// "no bound" sentinel inside Plan).
	sentinelStart := time.Unix(0, 1).UTC()
	sentinelEnd := time.Unix(0, 2).UTC()

	plan, err := Plan(expanded, customerID, "", sentinelStart, sentinelEnd)
	if err != nil {
		return nil, fmt.Errorf("lower: plan: %w", err)
	}
	if plan.TraceStats != nil {
		// The trace list takes two queries; a lowered plan holds one.
		return nil, errors.New("lower: the recent-traces list runs in two steps and can't be lowered")
	}

	return &LoweredPlan{
		CustomerID:        customerID,
		Tier:              tier,
		ShardSQL:          plan.ShardSQL,
		DirectShardSQL:    plan.DirectShardSQL,
		ArgSources:        plan.ArgSources,
		ComputeStages:     plan.ComputeStages,
		RowStages:         plan.RowStages,
		StagesReadPayload: plan.StagesReadPayload,
		LiveStages:        plan.LiveStages,
		MergeKey:          plan.ShardOrderBy,
		Aggregate:         plan.Aggregate,
		Group:             plan.Group,
		Limit:             plan.Limit,
		ShardLimit:        plan.ShardLimit,
		TailMode:          plan.TailMode,
		Follow:            plan.Follow,
		FollowPatterns:    plan.FollowPatterns,
		SourceCmd:         plan.SourceCmd,
		SourceNamespace:   plan.SourceNamespace,
		SourceService:     plan.SourceService,
		SourceKind:        plan.SourceKind,
		AlertArgs:         plan.AlertArgs,
		RedirectTarget:    plan.RedirectTarget,
		RedirectAppend:    plan.RedirectAppend,
		BinDeps:           deps,
		CompiledAt:        time.Now().UTC(),
		PlannerVersion:    PlannerVersion,
	}, nil
}

// Hydrate produces the concrete dispatch values for one fire. Returns
// the args slice in the order the placeholders appear in ShardSQL /
// DirectShardSQL — pass directly to pb.ExecuteQueryRequest.Args
// (after the []string conversion the coordinator already does).
func (lp *LoweredPlan) Hydrate(ctx ResolveCtx) []any {
	args := make([]any, len(lp.ArgSources))
	for i, src := range lp.ArgSources {
		args[i] = src.Resolve(ctx)
	}
	return args
}

// HashBinSource returns the content hash format BinDep stores. Exposed
// so callers comparing live /bin source against a recorded BinDep
// compute hashes the same way Lower did.
func HashBinSource(source string) string {
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])
}

// LowerForStorage wraps Lower + json.Marshal for save handlers. On
// failure (unsupported pipeline shape, /bin not yet inlineable, JSON
// error) it logs and returns nil so the caller passes a NULL
// lowered_plan into SaveCronWatch — the watch saves cleanly and the
// fire path falls back to inline Plan(). This is the canonical entry
// point for save handlers; do NOT call Lower + Marshal directly.
func LowerForStorage(ctx context.Context, pipeline string, customerID int64, tier string, resolveBin BinResolver) []byte {
	lp, err := Lower(pipeline, customerID, tier, resolveBin)
	if err != nil {
		weftsdk.Infof(ctx, "lower: skip save-time lowering for pipeline (customer=%d): %v", customerID, err)
		return nil
	}
	b, err := json.Marshal(lp)
	if err != nil {
		weftsdk.Infof(ctx, "lower: marshal LoweredPlan (customer=%d): %v", customerID, err)
		return nil
	}
	return b
}

// expandForLowering is the save-time counterpart to coordinator's
// expandBinPipeline. It walks the pipeline's bash AST, finds the
// leading command, and if that's a /bin/<name> bash invocation,
// inlines the script body. Behavior mirrors expandBinPipeline so
// the save-time pre-inlined SQL matches what the fire-time fallback
// would produce — same shape constraints (single-pipeline body,
// SimpleCommand stages only, positional-arg binding), same fall-
// through ("can't splice this shape → leave the pipeline alone").
//
// Each successfully inlined /bin/<name> is recorded as a BinDep
// with the SHA-256 of its source. The fire path compares each dep's
// recorded hash against the live shell_functions row at every fire;
// a drift forces fallback + async re-lowering. This is the
// invalidation channel for /bin edits.
//
// Recursion: this is single-level expansion. A /bin/foo that itself
// invokes /bin/bar leaves the /bin/bar token in place — Plan() will
// reject the resulting "not a /log/... source" shape and Lower will
// return an error. The save handler treats that as "didn't lower,
// store NULL," and the fire path runs Plan() inline. Phase B v3
// could recurse, but the iteration cost is best paid only after
// telemetry shows nested /bin in production watches.
func expandForLowering(pipeline string, resolveBin BinResolver) (string, []BinDep, error) {
	if resolveBin == nil {
		return pipeline, nil, nil
	}
	ast, err := bash.Parse(pipeline)
	if err != nil {
		// Plan() will surface the parse error with its own context.
		return pipeline, nil, nil
	}
	if len(ast.Commands) == 0 {
		return pipeline, nil, nil
	}
	list := ast.Commands[0]
	if list == nil || list.First == nil || len(list.First.Commands) == 0 {
		return pipeline, nil, nil
	}
	leading, ok := list.First.Commands[0].(*bash.SimpleCommand)
	if !ok || len(leading.Words) == 0 {
		return pipeline, nil, nil
	}
	name := leading.Words[0].Literal()
	if name == "" {
		return pipeline, nil, nil
	}

	source, hash, ok := resolveBin.Source(name)
	if !ok {
		// Not a /bin/<name> script — leading command stays as-is. If
		// the result isn't /log/-source-shaped Plan will return an
		// error; the save handler swallows it and stores NULL.
		return pipeline, nil, nil
	}

	bodyAST, err := bash.Parse(source)
	if err != nil {
		return "", nil, fmt.Errorf("/bin/%s: parse body: %w", name, err)
	}
	if len(bodyAST.Commands) != 1 || len(bodyAST.Commands[0].Rest) != 0 {
		// Multi-list body can't be spliced cleanly. Fall through —
		// the fire path's expandBinPipeline has the same restriction.
		return pipeline, nil, nil
	}
	bodyList := bodyAST.Commands[0]
	if bodyList.First == nil {
		return pipeline, nil, nil
	}
	bodyCmds := bodyList.First.Commands
	if len(bodyCmds) == 0 {
		return pipeline, nil, nil
	}
	for _, c := range bodyCmds {
		if _, isSimple := c.(*bash.SimpleCommand); !isSimple {
			return pipeline, nil, nil
		}
	}

	// Bind positionals from the invocation into the body — same as
	// coordinator/bin_expand.go::expandBinPipeline. Literal-only args:
	// expansions like $VAR or `subshell` are not evaluated.
	args := make([]string, 0, len(leading.Words)-1)
	for _, w := range leading.Words[1:] {
		args = append(args, w.Literal())
	}
	bash.BindPositionals(bodyAST, args)

	// Splice the body's commands into the outer pipeline, replacing
	// the original /bin invocation. Subsequent stages of the outer
	// pipeline tail in unchanged.
	list.First.Commands = append(bodyCmds, list.First.Commands[1:]...)

	return bash.Unparse(ast), []BinDep{{Name: name, Hash: hash}}, nil
}
