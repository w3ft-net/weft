package setup

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/w3ft-net/weft/ingest"
)

// previewColors and previewLabels give each extracted value its 256-color
// highlight and its name in the key.
var previewColors = map[ingest.ProbeField]string{
	ingest.ProbeTimestamp:    "39",
	ingest.ProbeLevel:        "208",
	ingest.ProbeTraceID:      "78",
	ingest.ProbeSpanName:     "105",
	ingest.ProbeSpanID:       "214",
	ingest.ProbeParentSpanID: "209",
	ingest.ProbeDurationNs:   "80",
}

var previewLabels = map[ingest.ProbeField]string{
	ingest.ProbeTimestamp:    "timestamp",
	ingest.ProbeLevel:        "level",
	ingest.ProbeTraceID:      "trace ID",
	ingest.ProbeSpanName:     "span name",
	ingest.ProbeSpanID:       "span ID",
	ingest.ProbeParentSpanID: "parent span ID",
	ingest.ProbeDurationNs:   "duration",
}

// RenderPreview draws the preview's lines with each value in its own color,
// clipped to width, then a blank line and a key of the values that matched.
func RenderPreview(p *ingest.Preview, width int) string {
	return RenderPreviewWith(lipgloss.DefaultRenderer(), p, width)
}

// RenderPreviewWith is RenderPreview against an explicit renderer, so callers
// (and tests) control the color profile.
func RenderPreviewWith(r *lipgloss.Renderer, p *ingest.Preview, width int) string {
	var b strings.Builder
	for _, segs := range p.Lines {
		b.WriteString(renderPreviewLine(r, segs, width))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	muted := r.NewStyle().Foreground(lipgloss.Color("110"))
	keys := make([]string, 0, len(p.Fields))
	for _, f := range p.Fields {
		label := previewLabels[f]
		if label == "" {
			label = string(f)
		}
		sq := r.NewStyle().Foreground(lipgloss.Color(previewColors[f])).Render("■")
		keys = append(keys, sq+" "+muted.Render(label))
	}
	b.WriteString(strings.Join(keys, "  "))
	b.WriteByte('\n')
	return b.String()
}

// renderPreviewLine windows a line to width runes, scrolled so the first
// highlighted value stays visible (the same policy as renderRow).
func renderPreviewLine(r *lipgloss.Renderer, segs []ingest.PreviewSegment, width int) string {
	type cell struct {
		r rune
		f ingest.ProbeField
	}
	var cells []cell
	anchor := -1
	for _, s := range segs {
		if s.Field != "" && anchor < 0 {
			anchor = len(cells)
		}
		for _, c := range s.Text {
			cells = append(cells, cell{c, s.Field})
		}
	}
	start := 0
	if width > 0 && len(cells) > width {
		start = min(max(max(anchor, 0)-width/3, 0), len(cells)-width)
		cells = cells[start:min(start+width, len(cells))]
	}
	var b strings.Builder
	for i := 0; i < len(cells); {
		j := i
		var run strings.Builder
		for j < len(cells) && cells[j].f == cells[i].f {
			run.WriteRune(cells[j].r)
			j++
		}
		if f := cells[i].f; f != "" {
			b.WriteString(r.NewStyle().Foreground(lipgloss.Color(previewColors[f])).Bold(true).Render(run.String()))
		} else {
			b.WriteString(run.String())
		}
		i = j
	}
	return b.String()
}
