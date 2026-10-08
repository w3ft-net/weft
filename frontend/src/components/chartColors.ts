// Chart colours mirror the ANSI-ish tokens in index.css. Recharts writes
// them as SVG presentation attributes, where var() isn't reliable, so
// they're spelled out here. Keep in sync with :root.
export const CHART_COLORS = {
  line: '#5fafd7',     // --accent
  tick: '#555577',     // --text-dim
  grid: '#252540',     // --border
  warn: '#ffd75f',     // --yellow
  crit: '#ff5f5f',     // --red
  safe: 'rgba(95, 215, 135, 0.06)', // --green, faint
  surface: '#0f0f1a',  // --bg-surface
  text: '#c0c0d0',     // --text-primary
  border: '#3a3a60',   // --border-bright
}
