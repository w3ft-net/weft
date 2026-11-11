package planner

import (
	"fmt"
	"strings"
	"time"
)

// Placement is where a stage runs, in the terms a user sees: as part of
// the optimized retrieval, which narrows and reshapes rows as they are
// fetched so that only the result comes back, or post-retrieval, on rows
// already fetched.
type Placement int

const (
	Optimized Placement = iota
	// Partial: the stage's filter is part of the optimized retrieval and
	// the rest of the stage runs post-retrieval.
	Partial
	PostRetrieval
)

func (p Placement) String() string {
	switch p {
	case Optimized:
		return "optimized retrieval"
	case Partial:
		return "partly optimized"
	}
	return "post-retrieval"
}

// Advice says why a stage isn't entirely optimized retrieval and what, if
// anything, would make it so. It is written for the person typing the
// pipeline: it names no storage or execution internals.
type Advice struct {
	Because string
	Try     string
}

// StageReport is one stage after the source command.
type StageReport struct {
	Index int    // 1-based position after the source, as in the planner
	Src   string // the stage as the user wrote it
	Where Placement
	// Split says which parts of a partly optimized stage are which.
	Split string
	Why   *Advice // nil when the stage is optimized, or runs after an earlier barrier with nothing of its own to say
}

// Analysis is what a user can learn about where their pipeline runs. It
// is derived from the same compiled IR a run uses, so it describes what a
// run does.
type Analysis struct {
	Stages []StageReport
	// Barrier is the first stage not entirely optimized, and every
	// stage after it runs on the rows it returns. 0 means there is none.
	Barrier int
	// RetrievalCount is a pipeline that prints how many rows the optimized
	// retrieval returns (retrievalCount), empty when none is worth running.
	// The caller runs it, as it would any query of the user's, and reports
	// the result with Retrieved, or says why it couldn't in RowsNote.
	RetrievalCount string
	Rows           int64
	HasRows        bool
	RowsNote       string
}

// Analyze compiles a pipeline and reports where each stage runs. cwd
// resolves relative source paths, as for Plan. A leading /bin script is
// inlined through resolveBin, as a run does; nil leaves it as it is.
func Analyze(pipeline, cwd string, resolveBin BinResolver) (*Analysis, error) {
	pipeline, _, err := expandForLowering(pipeline, resolveBin)
	if err != nil {
		return nil, err
	}
	_, c, err := compileBare(pipeline, cwd, time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}
	if c == nil { // an alert with no query in front of it
		return &Analysis{}, nil
	}
	a := c.analyze()
	a.RetrievalCount = c.retrievalCount(pipeline, a.Barrier)
	return a, nil
}

// analyze derives each stage's placement from the lists a run uses: a
// stage still runs post-retrieval iff it's one of the row or compute
// stages, and part of it is optimized retrieval iff one of its nodes is in
// the shard.
func (c *compiled) analyze() *Analysis {
	runsAfter := map[*loweredStage]bool{}
	for _, s := range c.rowStages {
		runsAfter[s] = true
	}
	for _, s := range c.compute {
		runsAfter[s] = true
	}
	inRetrieval := map[int]bool{}
	lineCount := false // a head or tail count is applied during retrieval
	for _, n := range c.nodes[:c.shard] {
		inRetrieval[n.stageIndex()] = true
		if _, ok := n.(*limitNode); ok {
			lineCount = true
		}
	}

	a := &Analysis{}
	for _, s := range c.stages {
		r := StageReport{Index: s.index, Src: s.src}
		switch {
		case !runsAfter[s]:
			r.Where = Optimized
		case inRetrieval[s.index]:
			r.Where = Partial
		default:
			r.Where = PostRetrieval
		}
		if r.Where == Partial {
			r.Split = splitOf(s.cmd)
		}
		if r.Where != Optimized {
			r.Why = whyAfter(s, a.Barrier, lineCount)
			if a.Barrier == 0 {
				a.Barrier = s.index
			}
		}
		a.Stages = append(a.Stages, r)
	}
	return a
}

// splitOf says what is optimized and what is not in a partly optimized
// stage of the given command.
func splitOf(cmd string) string {
	switch cmd {
	case "awk":
		return "pattern: optimized; action: post-retrieval"
	case "jq":
		return "select: optimized; rest: post-retrieval"
	}
	return "filter: optimized; rest: post-retrieval"
}

// whyAfter explains a stage that isn't entirely optimized: its own reason
// if its frontend gave one, else that it filters the rows a head or tail
// kept. A stage after the barrier with nothing of its own to say gets no
// advice; the barrier line covers it.
func whyAfter(s *loweredStage, barrier int, lineCount bool) *Advice {
	filters := false
	for _, n := range s.nodes {
		switch n := n.(type) {
		case *opaqueNode:
			if n.why.code != reasonNone {
				return adviceFor(n.why, s.cmd)
			}
		case *aggregateNode:
			if n.why.code != reasonNone {
				return adviceFor(n.why, s.cmd)
			}
		case *filterNode, *limitNode:
			filters = true
		}
	}
	if filters && lineCount && barrier == 0 {
		return &Advice{
			Because: "Filters rows a head or tail already kept.",
			Try:     "Filter first: that gives the last N matches, not the matches among the last N.",
		}
	}
	if barrier != 0 {
		return nil
	}
	return &Advice{Because: "No optimized form."}
}

// adviceFor is the user-facing text for a reason a frontend left a stage
// opaque, or lowered it only partly. Every code has an entry: a test
// walks them all.
func adviceFor(r reason, cmd string) *Advice {
	switch r.code {
	case reasonNoFrontend:
		return &Advice{Because: fmt.Sprintf("%s: post-retrieval only.", r.detail)}
	case reasonFlag:
		return flagAdvice(cmd, r.detail)
	case reasonParse:
		return &Advice{Because: "Program doesn't parse.", Try: "Check the quoting."}
	case reasonAwkFunction:
		return &Advice{Because: "Function definitions: post-retrieval."}
	case reasonAwkBegin:
		return &Advice{
			Because: "BEGIN can change line splitting.",
			Try:     "Drop BEGIN; use awk's default splitting.",
		}
	case reasonAwkUnpatterned:
		return &Advice{
			Because: "Rule without a pattern: acts on every row.",
			Try:     "To select rows, add a pattern. Totals (n++, sums) are post-retrieval; to count, use `grep PATTERN | wc -l`.",
		}
	case reasonAwkNoMainRule:
		return &Advice{Because: "Only BEGIN or END blocks."}
	case reasonAwkEndReadsRecord:
		return &Advice{Because: "END reads the last line: needs every row."}
	case reasonAwkGetline:
		return &Advice{Because: "getline reads rows that narrowing would drop."}
	case reasonAwkRecordCounter:
		return &Advice{
			Because: "NR/FNR count rows: narrowing first changes the count.",
			Try:     "Use head or tail to take a window of rows.",
		}
	case reasonAwkComputedField:
		return &Advice{
			Because: "Computed field position ($i, $(n+1)).",
			Try:     "Use a fixed position ($1, $2) or the last field ($NF), or jq on a JSON source.",
		}
	case reasonAwkStrnum:
		return &Advice{Because: "Number comparison: awk picks number or text per value."}
	case reasonAwkTS:
		return &Advice{Because: "TS is text in awk: can't be compared."}
	case reasonAwkPattern:
		return &Advice{Because: "Pattern uses a construct that isn't optimized."}
	case reasonAwkRules:
		return &Advice{Because: "Several rules: a row matching any must come back."}
	case reasonAwkEnd:
		return &Advice{Because: "END runs after all rows."}
	case reasonAwkAction, reasonAwkProjection:
		return &Advice{
			Because: "Action does more than print the line or its fields.",
			Try:     "Print only, e.g. `print $1, HOST`.",
		}
	case reasonJqSlurp:
		return &Advice{Because: "-s reads the whole stream as one input."}
	case reasonJqSelect:
		return &Advice{Because: "Select uses a construct that isn't optimized."}
	case reasonJqTail:
		return &Advice{Because: "After the selects: more than picking fields."}
	case reasonGrepPattern:
		return &Advice{
			Because: "grep optimized with exactly one pattern.",
			Try:     "Chain greps to require several patterns.",
		}
	case reasonSedProgram:
		return &Advice{
			Because: "sed optimized only as `sed -n '/re/p'`.",
			Try:     "Matching lines: use grep. Substitution: post-retrieval, so put it after narrowing stages.",
		}
	case reasonCutArgs:
		return &Advice{Because: "cut optimized only as `-d DELIM -f LIST`."}
	case reasonCutFields:
		return &Advice{Because: "Field list must be N, N-M or N- items."}
	case reasonCutDelimiter:
		return &Advice{Because: "Empty delimiter splits into characters."}
	}
	return nil
}

// flagAdvice is the advice for a stage kept out of the optimized
// retrieval by a flag of its command.
func flagAdvice(cmd, flag string) *Advice {
	switch {
	case cmd == "grep":
		return &Advice{Because: fmt.Sprintf("grep optimized with -i, -v, or no flags; not %s.", flag)}
	case cmd == "awk" && strings.HasPrefix(flag, "-F"):
		return &Advice{
			Because: fmt.Sprintf("Flag %s changes line splitting.", flag),
			Try:     "Fixed separator: `cut -d<sep> -f<n>` is optimized (lines without the separator pass whole).",
		}
	case cmd == "uniq":
		return &Advice{Because: fmt.Sprintf("uniq optimized bare or with -c; not %s.", flag)}
	}
	return &Advice{Because: fmt.Sprintf("Flag %s: not optimized.", flag)}
}

const (
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[31m"
	ansiReset  = "\x1b[0m"
)

// label is a placement's name, colored for a terminal: green for optimized,
// yellow for partly, red for post-retrieval.
func (p Placement) label(color bool) string {
	if !color {
		return p.String()
	}
	c := ansiRed
	switch p {
	case Optimized:
		c = ansiGreen
	case Partial:
		c = ansiYellow
	}
	return c + p.String() + ansiReset
}

// Render is the analysis as text: each stage with where it runs and why,
// then the barrier. A fully optimized pipeline says so. color adds
// terminal colors to the placements; leave it off for text that travels
// anywhere else, such as a saved cron's warning.
func (a *Analysis) Render(color bool) string {
	var b strings.Builder
	if a.Barrier == 0 {
		return paint(color, ansiGreen, "Every stage: optimized retrieval.") + "\n"
	}
	width := 0
	for _, s := range a.Stages {
		width = max(width, min(len([]rune(oneLine(s.Src))), 56))
	}
	for _, s := range a.Stages {
		if s.Index == a.Barrier {
			b.WriteString(a.rowsLine())
		}
		src := truncate(oneLine(s.Src), 56)
		fmt.Fprintf(&b, "  %d  %-*s  %s\n", s.Index, width, src, s.Where.label(color))
		if s.Split != "" {
			fmt.Fprintf(&b, "       %s\n", s.Split)
		}
		if s.Why == nil {
			continue
		}
		fmt.Fprintf(&b, "       %s\n", s.Why.Because)
		if s.Why.Try != "" {
			fmt.Fprintf(&b, "       Try: %s\n", s.Why.Try)
		}
	}
	fmt.Fprintf(&b, "\nBarrier: stage %d.\n", a.Barrier)
	return b.String()
}

func paint(color bool, c, s string) string {
	if !color {
		return s
	}
	return c + s + ansiReset
}

// oneLine collapses a stage's whitespace, so a program written over
// several lines is one row of the report.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
