package main

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	gmtext "github.com/yuin/goldmark/text"
)

// ANSI codes that match shellcore/ansi.go. We embed the raw escape
// sequences rather than reference the package constants so the
// generated file has no runtime dependency on string-assembly order.
const (
	ansiAccent = "\x1b[38;5;208m"
	ansiDim    = "\x1b[38;5;8m"
	ansiReset  = "\x1b[0m"
)

// renderShell converts a markdown body to the plain-text-plus-ANSI form
// that the in-shell `less` pager renders. The document's H1 is skipped
// (the title is already in the help index / banner).
func renderShell(body []byte, banner string) string {
	root := goldmark.New(goldmark.WithExtensions(extension.Table)).Parser().Parse(gmtext.NewReader(body))

	r := shellRenderer{src: body}
	if banner != "" {
		r.buf.WriteString(formatBanner(banner))
		r.buf.WriteString("\n\n")
	}
	r.renderChildren(root)
	out := r.buf.String()
	// Collapse any trailing blank runs down to exactly one newline.
	return strings.TrimRight(out, "\n") + "\n"
}

// formatBanner renders the man-page top banner: the name on the left
// and right, the literal "Weft Commands" centered, padded out to 78
// columns. Matches the existing ALERT(1)/LESS(1) banners.
func formatBanner(name string) string {
	const width = 78
	const centerLabel = "Weft Commands"
	leftRight := name
	// If the name has no section (e.g. "ALERT"), assume section 1.
	if !strings.Contains(name, "(") {
		leftRight = name + "(1)"
	}
	totalPad := width - 2*len(leftRight) - len(centerLabel)
	if totalPad < 2 {
		return leftRight + "  " + centerLabel + "  " + leftRight
	}
	leftPad := totalPad / 2
	rightPad := totalPad - leftPad
	return leftRight + strings.Repeat(" ", leftPad) + centerLabel + strings.Repeat(" ", rightPad) + leftRight
}

type shellRenderer struct {
	buf    bytes.Buffer
	src    []byte
	indent int // current indent level (spaces)
}

func (r *shellRenderer) renderChildren(n ast.Node) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		r.renderBlock(c)
	}
}

func (r *shellRenderer) renderBlock(n ast.Node) {
	switch n := n.(type) {
	case *ast.Heading:
		r.renderHeading(n)
	case *ast.Paragraph:
		r.renderParagraph(n)
	case *ast.FencedCodeBlock:
		r.renderCodeBlock(n)
	case *ast.CodeBlock:
		r.renderCodeBlock(n)
	case *ast.List:
		r.renderList(n)
	case *ast.Blockquote:
		r.renderBlockquote(n)
	case *east.Table:
		r.renderTable(n)
	case *ast.ThematicBreak:
		r.buf.WriteString("\n")
	default:
		// Unknown block — render children and hope. Keeps the tool
		// from silently dropping extensions we might add later.
		r.renderChildren(n)
	}
}

func (r *shellRenderer) renderHeading(n *ast.Heading) {
	text := collectText(n, r.src)
	switch n.Level {
	case 1:
		// H1 is the title; frontmatter already carries it and the
		// banner (if any) has been emitted. Skip.
	case 2:
		r.buf.WriteString(ansiAccent)
		r.buf.WriteString(text)
		r.buf.WriteString(ansiReset)
		r.buf.WriteString("\n")
	default:
		// H3 and below render as a dimmed label, two-space indented.
		r.buf.WriteString("  ")
		r.buf.WriteString(ansiAccent)
		r.buf.WriteString(text)
		r.buf.WriteString(ansiReset)
		r.buf.WriteString("\n")
	}
}

// renderBlockquote renders a blockquote as dim-gray text. Used for
// introductory lines and tips in help pages (the "weft shell" header
// and the "Tip: long output is paged automatically" footer).
func (r *shellRenderer) renderBlockquote(n *ast.Blockquote) {
	text := collectInline(n, r.src)
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line == "" {
			r.buf.WriteString("\n")
			continue
		}
		r.buf.WriteString(strings.Repeat(" ", r.indent))
		r.buf.WriteString(ansiDim)
		r.buf.WriteString(line)
		r.buf.WriteString(ansiReset)
		r.buf.WriteString("\n")
	}
	r.buf.WriteString("\n")
}

func (r *shellRenderer) renderParagraph(n *ast.Paragraph) {
	text := collectInline(n, r.src)
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			r.buf.WriteString("\n")
			continue
		}
		r.buf.WriteString(strings.Repeat(" ", r.indent+2))
		r.buf.WriteString(line)
		r.buf.WriteString("\n")
	}
	r.buf.WriteString("\n")
}

func (r *shellRenderer) renderCodeBlock(n ast.Node) {
	var lines []string
	for i := 0; i < n.Lines().Len(); i++ {
		seg := n.Lines().At(i)
		line := string(seg.Value(r.src))
		// Lines already carry their terminator; trim it so we can
		// indent uniformly and re-emit.
		line = strings.TrimRight(line, "\n")
		lines = append(lines, line)
	}
	pad := strings.Repeat(" ", r.indent+4)
	for _, line := range lines {
		if line == "" {
			r.buf.WriteString("\n")
			continue
		}
		r.buf.WriteString(pad)
		r.buf.WriteString(line)
		r.buf.WriteString("\n")
	}
	r.buf.WriteString("\n")
}

func (r *shellRenderer) renderList(n *ast.List) {
	ordered := n.IsOrdered()
	counter := n.Start
	if counter == 0 {
		counter = 1
	}
	for item := n.FirstChild(); item != nil; item = item.NextSibling() {
		li, ok := item.(*ast.ListItem)
		if !ok {
			continue
		}
		var marker string
		if ordered {
			marker = fmt.Sprintf("%d. ", counter)
			counter++
		} else {
			marker = "- "
		}
		r.renderListItem(li, marker)
	}
	// Trailing blank line separates the list from whatever comes
	// next — without it a follow-up paragraph reads as another item.
	r.buf.WriteString("\n")
}

func (r *shellRenderer) renderListItem(li *ast.ListItem, marker string) {
	pad := strings.Repeat(" ", r.indent+2)
	contPad := pad + strings.Repeat(" ", len(marker))

	first := true
	for c := li.FirstChild(); c != nil; c = c.NextSibling() {
		switch child := c.(type) {
		case *ast.TextBlock, *ast.Paragraph:
			text := collectInline(child, r.src)
			lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
			for i, line := range lines {
				if i == 0 && first {
					r.buf.WriteString(pad)
					r.buf.WriteString(marker)
				} else {
					r.buf.WriteString(contPad)
				}
				r.buf.WriteString(line)
				r.buf.WriteString("\n")
			}
			first = false
		case *ast.List:
			// Nested list: indent the child's render by the marker
			// width so it sits under the parent item text.
			saved := r.indent
			r.indent += 2 + len(marker)
			r.renderList(child)
			r.indent = saved
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			saved := r.indent
			r.indent += len(marker)
			r.renderCodeBlock(child)
			r.indent = saved
		default:
			r.renderBlock(c)
		}
	}
}

// collectText returns the concatenated plain text of a node and its
// descendants, with no formatting stripped beyond what's needed to
// produce printable terminal text.
func collectText(n ast.Node, src []byte) string {
	var b strings.Builder
	walkInline(n, src, &b)
	return b.String()
}

// collectInline is the same as collectText for now; split into its
// own name for readability in the block renderers.
func collectInline(n ast.Node, src []byte) string { return collectText(n, src) }

// walkInline flattens inline formatting. We intentionally strip
// emphasis, bold, and inline-code markup — the terminal output doesn't
// carry them, and doubling up backticks around already-monospace
// content isn't useful. Links render as "text (url)" when the two
// differ, else just the bare url.
func walkInline(n ast.Node, src []byte, b *strings.Builder) {
	switch node := n.(type) {
	case *ast.Text:
		seg := node.Segment
		b.Write(seg.Value(src))
		if node.SoftLineBreak() || node.HardLineBreak() {
			b.WriteString("\n")
		}
	case *ast.String:
		b.Write(node.Value)
	case *ast.CodeSpan:
		for c := node.FirstChild(); c != nil; c = c.NextSibling() {
			walkInline(c, src, b)
		}
	case *ast.Emphasis:
		for c := node.FirstChild(); c != nil; c = c.NextSibling() {
			walkInline(c, src, b)
		}
	case *ast.Link:
		var label strings.Builder
		for c := node.FirstChild(); c != nil; c = c.NextSibling() {
			walkInline(c, src, &label)
		}
		txt := label.String()
		dest := string(node.Destination)
		if txt == "" || txt == dest {
			b.WriteString(dest)
		} else {
			b.WriteString(txt)
			b.WriteString(" (")
			b.WriteString(dest)
			b.WriteString(")")
		}
	case *ast.AutoLink:
		b.Write(node.URL(src))
	default:
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			walkInline(c, src, b)
		}
	}
}

// writeShell writes the generated Go file containing one const per
// shell-enabled doc plus a dispatch map keyed by topic string.
func writeShell(path string, docs []*doc) error {
	var buf bytes.Buffer
	buf.WriteString("// Code generated by cmd/docgen. DO NOT EDIT.\n\n")
	buf.WriteString("package shell\n\n")

	type entry struct {
		constName string
		topics    []string
		id        string
	}
	var entries []entry

	for _, d := range docs {
		if d.Shell == nil {
			continue
		}
		if d.Shell.Const == "" {
			return fmt.Errorf("%s: shell.const is required", d.path)
		}
		text := renderShell(d.body, d.Shell.Banner)

		fmt.Fprintf(&buf, "// %s — %s (generated from %s).\n",
			d.Shell.Const, d.Title, d.path)
		fmt.Fprintf(&buf, "const %s = %s\n\n", d.Shell.Const, strconv.Quote(text))

		entries = append(entries, entry{d.Shell.Const, d.Shell.Topics, d.ID})
	}

	// Dispatch map: every topic string resolves to its const body.
	buf.WriteString("// shellDocTopics maps each user-typed dispatch string\n")
	buf.WriteString("// (e.g. \"help alerts\", \"man range\") to its rendered body.\n")
	buf.WriteString("var shellDocTopics = map[string]string{\n")
	for _, e := range entries {
		for _, t := range e.topics {
			fmt.Fprintf(&buf, "\t%s: %s,\n", strconv.Quote(t), e.constName)
		}
	}
	buf.WriteString("}\n")

	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// tableWidth is the column budget a rendered table must fit inside:
// an 80-column terminal less the two-space page margin.
const tableWidth = 78

// renderTable lays a GFM table out as aligned columns: dim header, a
// dashed rule, then the rows. When the natural width overflows the
// terminal the widest column is shrunk and its cells word-wrapped.
func (r *shellRenderer) renderTable(t *east.Table) {
	var rows [][]string
	headerRows := 0
	for row := t.FirstChild(); row != nil; row = row.NextSibling() {
		var cells []string
		for c := row.FirstChild(); c != nil; c = c.NextSibling() {
			cells = append(cells, strings.Join(strings.Fields(collectText(c, r.src)), " "))
		}
		if _, ok := row.(*east.TableHeader); ok {
			headerRows = len(rows) + 1
		}
		rows = append(rows, cells)
	}
	cols := 0
	for _, row := range rows {
		cols = max(cols, len(row))
	}
	if cols == 0 {
		return
	}
	widths := make([]int, cols)
	for _, row := range rows {
		for i, c := range row {
			widths[i] = max(widths[i], len(c))
		}
	}
	const gap = 2
	budget := tableWidth - r.indent - gap*(cols-1)
	for sum(widths) > budget {
		w := 0
		for i := range widths {
			if widths[i] > widths[w] {
				w = i
			}
		}
		if widths[w] <= 8 {
			break
		}
		widths[w]--
	}

	pad := strings.Repeat(" ", r.indent+2)
	emit := func(row []string, dim bool) {
		wrapped := make([][]string, cols)
		height := 1
		for i := range wrapped {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			wrapped[i] = wrapWords(cell, widths[i])
			height = max(height, len(wrapped[i]))
		}
		for l := 0; l < height; l++ {
			var line strings.Builder
			for i := range wrapped {
				part := ""
				if l < len(wrapped[i]) {
					part = wrapped[i][l]
				}
				if i < cols-1 {
					part += strings.Repeat(" ", max(widths[i]-len(part), 0)+gap)
				}
				line.WriteString(part)
			}
			r.buf.WriteString(pad)
			if dim {
				r.buf.WriteString(ansiDim)
			}
			r.buf.WriteString(strings.TrimRight(line.String(), " "))
			if dim {
				r.buf.WriteString(ansiReset)
			}
			r.buf.WriteString("\n")
		}
	}
	for i, row := range rows {
		emit(row, i < headerRows)
		if i == headerRows-1 {
			rule := make([]string, cols)
			for c := range rule {
				rule[c] = strings.Repeat("-", widths[c])
			}
			emit(rule, true)
		}
	}
	r.buf.WriteString("\n")
}

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

// wrapWords greedily wraps s to width columns; a single word longer
// than width is left whole rather than split.
func wrapWords(s string, width int) []string {
	var lines []string
	cur := ""
	for _, w := range strings.Fields(s) {
		switch {
		case cur == "":
			cur = w
		case len(cur)+1+len(w) <= width:
			cur += " " + w
		default:
			lines = append(lines, cur)
			cur = w
		}
	}
	return append(lines, cur)
}
