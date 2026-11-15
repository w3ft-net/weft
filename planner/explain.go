package planner

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Explain runs the planner on a pipeline and returns a human-readable
// breakdown: the IR (each node with the stage it came from, and why a
// stage stayed opaque or lowered only as a prefilter), what each pass
// did or declined, the shard SQL, and the compute stages. The tenant and
// time bounds are passed empty/zero — Explain is for human inspection, not
// dispatch, so the row-level security predicates are elided to keep the
// output focused on pipeline shape.
//
// The shard SQL is split at keyword boundaries, and the bound args are
// listed in render order so readers can correlate placeholders to args.
func Explain(pipeline string) string {
	plan, c, err := compileBare(pipeline, "", time.Time{}, time.Time{})
	if err != nil {
		return fmt.Sprintf("Error: %v\n", err)
	}

	var b strings.Builder

	fmt.Fprintf(&b, "Pipeline: %s\n", pipeline)

	// Alert-only pipeline carries AlertArgs but no SQL/source; render
	// the trailing alert and return.
	if c == nil {
		fmt.Fprintf(&b, "\nAlert-only pipeline:\n  alert %s\n", strings.Join(plan.AlertArgs, " "))
		return b.String()
	}

	fmt.Fprintf(&b, "\nPlan:\n")
	c.explainNodes(&b)

	if len(c.passes) > 0 {
		fmt.Fprintf(&b, "\nPasses:\n")
		for _, p := range c.passes {
			fmt.Fprintf(&b, "  %-20s %s\n", p.pass, p.msg)
		}
	}

	// Shard SQL — split into structured lines plus the args legend.
	fmt.Fprintf(&b, "\nShard SQL:\n")
	for _, line := range formatSQL(plan.ShardSQL) {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	if len(plan.Args) > 0 {
		fmt.Fprintf(&b, "\nBound args (in render order):\n")
		for i, a := range plan.Args {
			fmt.Fprintf(&b, "  $%d = %q\n", i+1, fmt.Sprintf("%v", a))
		}
	}

	// Every compute line is a stage the user typed, verbatim: the
	// planner never writes source.
	if len(c.rowStages) > 0 {
		fmt.Fprintf(&b, "\nRow stages (run per row; rows pass through whole):\n")
		for i, s := range c.rowStages {
			fmt.Fprintf(&b, "  %d. %s  [stage %d, verbatim]\n", i+1, s.src, s.index)
		}
	}
	if len(c.compute) > 0 {
		fmt.Fprintf(&b, "\nCompute stages:\n")
		for i, s := range c.compute {
			fmt.Fprintf(&b, "  %d. %s  [stage %d, verbatim]\n", i+1, s.src, s.index)
		}
		if plan.StagesReadPayload {
			fmt.Fprintf(&b, "  (stage 1 reads each row's payload)\n")
		}
	} else if len(c.rowStages) == 0 {
		fmt.Fprintf(&b, "\nCompute stages: (none — fully pushed down)\n")
	}

	return b.String()
}

// explainNodes renders the IR: the scan, then every node with its span,
// and the line where the shard hands rows to compute.
func (c *compiled) explainNodes(b *strings.Builder) {
	type line struct{ kind, desc, span, note string }
	lines := []line{{kind: "Scan", desc: c.scanDesc()}}
	for i, n := range c.nodes {
		if i == c.shard {
			lines = append(lines, line{kind: "───"})
		}
		l := line{kind: nodeKind(n), span: "[" + c.spanLabel(n) + "]"}
		switch n := n.(type) {
		case *filterNode:
			if n.exact {
				l.desc = "exact      " + n.pred
				if n.hoisted {
					l.note = "on each line"
				}
			} else {
				l.desc = "prefilter  " + n.pred
				l.note = "not exact: " + n.why.String()
			}
		case *limitNode:
			l.desc = strconv.Itoa(n.n)
			if n.offset > 0 {
				l.desc += fmt.Sprintf(" offset %d", n.offset)
			}
			if n.order != "" {
				l.desc += "  " + n.order
			}
			// A head the aggregate pass folded into the group.
			if note, ok := c.aggNotes[n]; ok {
				l.note = note
			}
		case *projectNode:
			l.desc = n.expr
			if n == c.selected {
				l.note = "the SELECT"
			}
		case *sortNode, *uniqNode, *countNode, *aggregateNode:
			l.desc = c.stage(n).src
			if slices.Contains(c.aggregated, n) {
				l.note = "the GROUP BY: " + string(c.group.Render)
				if note, ok := c.aggNotes[n]; ok {
					l.note = note
				}
			}
		case *opaqueNode:
			l.desc = c.stage(n).src
			l.note = n.why.String()
		}
		lines = append(lines, l)
	}
	if c.shard == len(c.nodes) {
		lines = append(lines, line{kind: "───"})
	}

	width := 0
	for _, l := range lines {
		width = max(width, min(len(l.desc), 56))
	}
	for _, l := range lines {
		if l.kind == "───" {
			fmt.Fprintf(b, "  ─── shard │ compute ───\n")
			continue
		}
		b.WriteString(strings.TrimRight(fmt.Sprintf("  %-8s %-*s  %s", l.kind, width, l.desc, l.span), " ") + "\n")
		if l.note != "" {
			fmt.Fprintf(b, "           %s\n", l.note)
		}
	}
}

// scanDesc names what the scan reads: the source's columns, or the
// trace it follows.
func (c *compiled) scanDesc() string {
	src := c.source
	switch src.cmd {
	case "trace":
		return "trace_id=" + src.traceID
	case "trace_spans":
		return "spans " + orAll(src.spanName)
	case "trace_list":
		return "recent traces"
	}
	desc := fmt.Sprintf("namespace=%s service=%s kind=%s",
		orAll(strings.Join(src.namespaces, ",")),
		orAll(strings.Join(src.services, ",")),
		orAll(strings.Join(src.kinds, ",")))
	if src.env != "" {
		desc += " env=" + src.env
	}
	if src.follow {
		desc += " follow"
	}
	return desc
}

// formatSQL breaks a SQL string into indented lines at keyword boundaries
// for readability. The split is purely textual — it doesn't traverse the
// AST — so this works correctly on both legacy baked-literal SQL and the
// new placeholder form. Tail's subquery flip is rendered with the inner
// SELECT on its own line; the closing `) AS t` lands as a separate clause
// so the nesting is visible.
func formatSQL(sql string) []string {
	keywords := []string{" WHERE ", " AND ", " GROUP BY ", " HAVING ", " ORDER BY ", " LIMIT ", " OFFSET ", " FROM (", ") AS t "}
	result := sql
	for _, kw := range keywords {
		result = strings.ReplaceAll(result, kw, "\n"+strings.TrimLeft(kw, " "))
	}
	return strings.Split(result, "\n")
}

// orAll renders an empty source-coordinate as the wildcard label for
// the human-readable EXPLAIN output.
func orAll(s string) string {
	if s == "" {
		return "(all)"
	}
	return s
}
