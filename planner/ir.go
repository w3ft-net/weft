package planner

import "fmt"

// The planner compiles a pipeline in three steps (docs/plans/planner-ir.md):
//
//  1. Frontends lower each stage, from its own syntax tree, into a few IR
//     nodes (frontend.go). A construct a frontend doesn't understand
//     becomes an opaqueNode, which runs the stage verbatim in compute.
//  2. Passes decide which nodes the shard computes (passes.go): filter
//     pushdown and hoisting, projection pushdown, aggregate recognition,
//     and sort/limit fusion.
//  3. Backends render the shard nodes through the Query builder and the
//     rest as compute stages, each the text of the stage it came from.
//
// No step reads a compute string: every decision is made on nodes, and a
// compute stage is only ever a stage the user typed.

// node is one IR node. Every node knows the stage it came from (its
// span), so the compute backend emits that stage's own text and Explain
// can line each node up with what the user typed.
type node interface {
	stageIndex() int
}

// at is a node's span: the position of the stage it came from, 0 for the
// source command and 1.. for the stages after it.
type at int

func (a at) stageIndex() int { return int(a) }

// filterNode keeps the rows where pred holds. An exact filter is the
// whole stage, so the stage drops out of compute when the shard runs it.
// A prefilter is only implied by the stage: true wherever the stage
// keeps a row, so it's safe in the shard as long as the stage still runs.
type filterNode struct {
	at
	pred  string
	exact bool
	why   reason // why a prefilter isn't exact
	// typed are the comparisons in pred that have a simpler form on a shard
	// where their locator is all numbers (see typedLeaf).
	typed []typedLeaf
	// follow is the grep pattern live tail applies when an exact grep
	// filter lands in the shard.
	follow *GrepPattern
	// lineWise: pred reads only raw, so on one line it's what the stage
	// does to that line (grep, sed -n). hoisted: filter hoisting moved
	// it below a sort, and the shard applies it to each line.
	lineWise, hoisted bool
}

// limitNode keeps n rows from offset: head/tail (order "ts ASC"/
// "ts DESC") or an awk NR window (order "", the scan's own).
type limitNode struct {
	at
	n, offset int
	order     string
}

// projectNode replaces each row's text with expr, which SQL computes
// exactly. It's always the last node of its stage.
type projectNode struct {
	at
	expr string
}

// sortNode is a sort stage. The shard pre-orders its rows by it
// (sort/limit fusion), or, ahead of a uniq or with -u, it's part of an
// aggregate. keyed is a flag that compares something other than the
// line's bytes (-n, -k, ...).
type sortNode struct {
	at
	reverse bool
	unique  bool // -u
	keyed   bool
	// numeric is -n, with no other flag or operand beyond -r and -u:
	// `sort -rn` over uniq -c text compares the count in front.
	numeric bool
}

// uniqNode is a uniq stage, with -c or without flags. Right after a
// sort it's part of an aggregate; otherwise it runs in compute.
type uniqNode struct {
	at
	count bool
}

// countNode is `wc -l`, or `wc -c` with bytes: an aggregate on its own.
type countNode struct {
	at
	bytes bool
}

// tallyNode is an awk count per key (awkTally): key is the SQL for the
// key. It is an aggregate on its own, like countNode; why says why the stage
// would run in compute if the aggregate isn't fused.
type tallyNode struct {
	at
	key string
	why reason
}

// opaqueNode runs its stage, verbatim, in compute.
type opaqueNode struct {
	at
	why reason
}

// loweredStage is a stage after its frontend ran.
type loweredStage struct {
	stageInfo
	index int // 1-based position after the source
	nodes []node
	// rowwise: the stage passes rows through unchanged, so the stage
	// after it still reads rows (docs/plans/planner-ir.md, "Row identity").
	rowwise bool
	// readsMeta: the stage names a meta-variable, so the shard projects
	// MetaVarColumns for compute to hand it.
	readsMeta bool
}

// readsRowMetadata reports whether a stage reads a row's payload or
// meta-variables: any jq (it reads the payload), or an awk naming a
// meta-variable.
func (s *loweredStage) readsRowMetadata() bool {
	return s.cmd == "jq" || (s.cmd == "awk" && s.readsMeta)
}

// reasonCode says why a frontend left a stage opaque, or lowered it only
// as a prefilter. Typed so tests assert on the code, not on wording.
type reasonCode int

const (
	reasonNone reasonCode = iota
	reasonNoFrontend
	reasonFlag
	reasonParse
	reasonAwkFunction
	reasonAwkBegin
	reasonAwkUnpatterned
	reasonAwkNoMainRule
	reasonAwkEndReadsRecord
	reasonAwkGetline
	reasonAwkRecordCounter
	reasonAwkComputedField
	reasonAwkStrnum
	reasonAwkTS
	reasonAwkPattern
	reasonAwkRules
	reasonAwkEnd
	reasonAwkAction
	reasonAwkProjection
	reasonJqSlurp
	reasonJqSelect
	reasonJqTail
	reasonGrepPattern
	reasonSedProgram
	reasonCutArgs
	reasonCutFields
	reasonCutDelimiter

	// reasonCodeCount ends the list: every code below it needs advice
	// (see adviceFor).
	reasonCodeCount
)

// reason is a reasonCode with the detail its message names (a flag, a
// command).
type reason struct {
	code   reasonCode
	detail string
}

func because(code reasonCode) reason { return reason{code: code} }

func (r reason) String() string {
	switch r.code {
	case reasonNoFrontend:
		return fmt.Sprintf("no SQL lowering for %s", r.detail)
	case reasonFlag:
		return fmt.Sprintf("flag %s isn't analyzed", r.detail)
	case reasonParse:
		return "the program doesn't parse"
	case reasonAwkFunction:
		return "defines a function"
	case reasonAwkBegin:
		return "BEGIN can change how records split"
	case reasonAwkUnpatterned:
		return "a rule without a pattern runs on every row"
	case reasonAwkNoMainRule:
		return "only BEGIN/END rules"
	case reasonAwkEndReadsRecord:
		return "END reads the last record"
	case reasonAwkGetline:
		return "getline reads rows the shard would filter out"
	case reasonAwkRecordCounter:
		return "NR/FNR outside an NR window: a filter changes what they count"
	case reasonAwkComputedField:
		return "reads a field by a computed index ($i, $(n+1))"
	case reasonAwkStrnum:
		return "compares with a number: awk compares as numbers or as strings by the value"
	case reasonAwkTS:
		return "TS is text in awk but a TIMESTAMP in SQL"
	case reasonAwkPattern:
		return "the pattern has no SQL lowering"
	case reasonAwkRules:
		return "more than one rule; the prefilter ORs their patterns"
	case reasonAwkEnd:
		return "an END rule runs after the rows"
	case reasonAwkAction:
		return "the action isn't the default print"
	case reasonAwkProjection:
		return "the action isn't a print SQL computes"
	case reasonJqSlurp:
		return "-s reads the whole stream as one input"
	case reasonJqSelect:
		return "a select has no SQL lowering"
	case reasonJqTail:
		return "after its selects, the program has no SQL lowering"
	case reasonGrepPattern:
		return "not a single pattern"
	case reasonSedProgram:
		return "only sed -n '/re/p' lowers"
	case reasonCutArgs:
		return "only cut -d D -f LIST lowers"
	case reasonCutFields:
		return "the field list isn't N, N-M or N- items"
	case reasonCutDelimiter:
		return "an empty delimiter splits into characters"
	}
	return ""
}
