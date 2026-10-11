import { colorForTag, formatSparkValue, type SparkSeries } from '../components/Sparkline'

// ChartPlot mirrors chart.Plot (chart/plot.go): a `... | chart` pipeline already
// grouped, bucketed and aggregated by the shell, ready to draw.
export interface ChartPlot {
  kind: 'line' | 'bar' | 'sparkline'
  timeX: boolean // x is unix seconds
  series: Array<{
    tag: string
    points: Array<{ x: number; label: string; y: number }>
  }>
}

// plotToSpark converts a plot to Sparkline's series shape. A time axis is
// unix seconds on the wire and milliseconds in Sparkline; an ordinal axis is
// passed through as-is.
export function plotToSpark(plot: ChartPlot): SparkSeries[] {
  return plot.series.map(s => {
    const points = s.points.map(p => ({ ts: plot.timeX ? p.x * 1000 : p.x, value: p.y }))
    return {
      tag: s.tag,
      color: colorForTag(s.tag),
      points,
      latest: points.length ? formatSparkValue(points[points.length - 1].value) : '',
    }
  })
}
