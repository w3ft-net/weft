// traceColors.ts — the CSS hex equivalents of theme/trace.go's
// traceTagColors ANSI-256 rotation (rose, sky, violet, olive,
// turquoise, orchid, amber, lime), in the same order so a
// TraceRow/SpanOccurrence's ColorIdx (theme.TraceLabelColorIndex —
// see docs/plans/browser-span-view.md) indexes straight into this
// array. A deliberate, bounded exception to "no duplicated logic
// across languages": this is a fixed 8-entry data table, not math —
// the hashing itself stays in Go (theme.TraceLabelColorIndex) and
// only its output crosses the WASM↔JS boundary as a plain int.
export const TRACE_COLORS: readonly string[] = [
  '#ff005f', // rose   (ANSI 197)
  '#5fd7ff', // sky    (ANSI 81)
  '#af87ff', // violet (ANSI 141)
  '#afd700', // olive  (ANSI 148)
  '#00ffff', // turquoise (ANSI 51)
  '#ff87ff', // orchid (ANSI 213)
  '#d78700', // amber  (ANSI 172)
  '#87ff00', // lime   (ANSI 118)
]

export function traceColorFor(colorIdx: number): string {
  return TRACE_COLORS[colorIdx] ?? TRACE_COLORS[0]
}
