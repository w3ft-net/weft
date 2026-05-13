package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// PlannerVersion stamps every LoweredPlan with the version of the
// planner that produced it. Bump this on any change to Plan() that
// would alter the SQL shape for a given pipeline (subquery wrap,
// merge-key flip, predicate pushdown rule). A version mismatch at
// fire time forces a recompile.
const PlannerVersion = 1

// LoweredPlan is the persisted shape we store next to a watch's
// pipeline string. It captures everything the runtime needs to
// dispatch the query without re-running Plan() — the SQL strings,
// the arg sources (so customer + time bounds can be re-resolved
// against fresh fire context), the compute stages, and the
// metadata that watches/dashboards consume (alert args, source
// app, follow flags). BinDeps records each /bin/ inlined during
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
	Customer        string      `json:"customer"`
	ShardSQL        string      `json:"shard_sql"`
	DirectShardSQL  string      `json:"direct_shard_sql"`
	ArgSources      []ArgSource `json:"args,omitempty"`
	ComputeStages   []string    `json:"compute_stages,omitempty"`
	LiveStages      []string    `json:"live_stages,omitempty"`
	MergeKey        string      `json:"merge_key,omitempty"`
	Limit           int         `json:"limit,omitempty"`
	ShardLimit      int         `json:"shard_limit,omitempty"`
	TailMode        bool        `json:"tail_mode,omitempty"`
	Follow          bool        `json:"follow,omitempty"`
	FollowApp       string      `json:"follow_app,omitempty"`
	FollowPatterns  []GrepPattern `json:"follow_patterns,omitempty"`
	SourceCmd       string      `json:"source_cmd,omitempty"`
	SourceApp       string      `json:"source_app,omitempty"`
	SourceNamespace string      `json:"source_namespace,omitempty"`
	AlertArgs       []string    `json:"alert_args,omitempty"`
	RedirectTarget  string      `json:"redirect_target,omitempty"`
	RedirectAppend  bool        `json:"redirect_append,omitempty"`
	BinDeps         []BinDep    `json:"bin_deps,omitempty"`
	CompiledAt      time.Time   `json:"compiled_at"`
	PlannerVersion  int         `json:"planner_version"`
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
	Customer string
	Start    time.Time
	End      time.Time
}

// Resolve produces the concrete value for one ArgSource. For
// ArgLiteral the value comes from the lowered form itself; the
// runtime kinds (Customer, TimeStart, TimeEnd) read from ctx.
func (a ArgSource) Resolve(ctx ResolveCtx) string {
	switch a.Kind {
	case ArgLiteral:
		return a.Value
	case ArgCustomer:
		return ctx.Customer
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
// (re-resolved at each fire from the watch's stored CustomerID).
//
// resolveBin reads /bin/<name> source for top-level bash inlining.
// Pass a no-op resolver if the caller doesn't need /bin expansion.
//
// The returned LoweredPlan is independent of customer/time-range
// values — passing a different customer at fire time produces
// different dispatch args but the same SQL string.
//
// Time bounds are not supplied here. Lower passes a sentinel time
// range into Plan() so the ts predicates get emitted with
// ArgTimeStart / ArgTimeEnd sources; Hydrate replaces the Values
// at fire time.
func Lower(pipeline, customer string, resolveBin BinResolver) (*LoweredPlan, error) {
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

	plan, err := Plan(expanded, customer, sentinelStart, sentinelEnd)
	if err != nil {
		return nil, fmt.Errorf("lower: plan: %w", err)
	}

	return &LoweredPlan{
		Customer:        customer,
		ShardSQL:        plan.ShardSQL,
		DirectShardSQL:  plan.DirectShardSQL,
		ArgSources:      plan.ArgSources,
		ComputeStages:   plan.ComputeStages,
		LiveStages:      plan.LiveStages,
		MergeKey:        plan.ShardOrderBy,
		Limit:           plan.Limit,
		ShardLimit:      plan.ShardLimit,
		TailMode:        plan.TailMode,
		Follow:          plan.Follow,
		FollowApp:       plan.FollowApp,
		FollowPatterns:  plan.FollowPatterns,
		SourceCmd:       plan.SourceCmd,
		SourceApp:       plan.SourceApp,
		SourceNamespace: plan.SourceNamespace,
		AlertArgs:       plan.AlertArgs,
		RedirectTarget:  plan.RedirectTarget,
		RedirectAppend:  plan.RedirectAppend,
		BinDeps:         deps,
		CompiledAt:      time.Now().UTC(),
		PlannerVersion:  PlannerVersion,
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
func LowerForStorage(pipeline, customer string, resolveBin BinResolver) []byte {
	lp, err := Lower(pipeline, customer, resolveBin)
	if err != nil {
		log.Printf("lower: skip save-time lowering for pipeline (customer=%s): %v", customer, err)
		return nil
	}
	b, err := json.Marshal(lp)
	if err != nil {
		log.Printf("lower: marshal LoweredPlan (customer=%s): %v", customer, err)
		return nil
	}
	return b
}

// expandForLowering is the lowering-side counterpart to coordinator's
// expandBinPipeline. It inlines top-level /bin/ bash invocations and
// records each inlined name + content hash so the lowered plan can
// detect /bin edits at fire time. Returns the expanded pipeline + the
// list of inlined bin deps.
//
// Today this delegates to a tiny scan-and-substitute pass that
// matches the same shape expandBinPipeline does. A future revision
// could share the expandBinPipeline implementation by lifting it out
// of the coordinator package — for now duplicating the small surface
// avoids the cross-package dep.
//
// IMPORTANT: this function does NOT recurse through nested /bin/ →
// /bin/ references the way expandBinPipeline does for the watch
// path. Phase B's scope is watches with at most one level of /bin
// inlining; deeper nesting falls back to per-fire Plan(). The
// fall-back is the same path the lowered-plan-miss branch uses.
func expandForLowering(pipeline string, resolveBin BinResolver) (string, []BinDep, error) {
	// Phase B v1: no inlining yet — leave the pipeline untouched and
	// record zero deps. Plan() will see /bin/foo as a literal token
	// and treat it as a non-log source (returns ErrNotPlannable),
	// which Lower surfaces as an error.
	//
	// This is a deliberate stub: it lets the rest of Phase B land
	// without entangling with coordinator/bin_expand.go's bash AST
	// walk. The proper inlining (and BinDep tracking) lands as a
	// follow-on commit before the fire-path switch — the
	// SaveCronWatch path can refuse to save when expandForLowering
	// can't fully lower a pipeline, falling back to "lowered_plan is
	// NULL, fire path uses Plan() inline."
	_ = resolveBin
	return pipeline, nil, nil
}
