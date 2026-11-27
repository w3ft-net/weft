// traceExports.ts — typed wrappers around the wasmParseTraceRows /
// wasmRenderSpanRollup stateless WASM exports cmd/wasm/main.go
// registers. Both wrap theme package math (interval-union self-time,
// exact percentiles, gap computation) directly — see
// docs/plans/browser-span-view.md — so there's exactly one
// implementation of that logic, not a JS reimplementation.
//
// Field names below match the Go structs' exported field names
// verbatim (theme.TraceRow / theme.SpanRollupRow / theme.SpanOccurrence
// carry no json tags, so encoding/json emits the Go names as-is).
// Duration fields are plain nanosecond counts (time.Duration marshals
// as its underlying int64) — see formatDurationNs in ShellTraceModal.

declare global {
  interface Window {
    wasmParseTraceRows: (lines: string[]) => string
    wasmRenderSpanRollup: (lines: string[]) => string
    wasmRenderSpanAggregate: (lines: string[]) => string
    wasmRenderSpanSamples: (lines: string[]) => string
    wasmParseTraceList: (lines: string[], nowMs: number) => string
    wasmSamplesPipeline: (aggregate: string, name: string, label: string) => string
    wasmTraceIDPipeline: (id: string) => string
  }
}

export interface TraceRow {
  Raw: string
  IsData: boolean
  TsNs: number
  Label: string
  Message: string
  GapNs: number
  Gap: string // GapNs as text ("+24ms"); empty for the first data row
  ColorIdx: number
}

// One line of a bare `trace`, the recent-traces list (theme.TraceListRow).
export interface TraceListRow {
  TraceID: string
  StartNs: number
  EndNs: number
  DurNs: number
  Dur: string // DurNs as text ("1.2s")
  Age: string // since the trace's last line ("3m")
  Lines: number
  Errors: number
  NoLevel: boolean // no line carried a log level, so Errors is unknown, not zero
  Services: string[] | null
  Root: string
}

export interface SpanOccurrence {
  Label: string
  TraceID: string
  SpanID: string
  ParentSpanID: string
  Duration: number // nanoseconds
  SelfDuration: number // nanoseconds
  Status: string
}

export interface SpanRollupRow {
  Name: string
  Label: string
  Calls: number
  Occurrences: SpanOccurrence[] | null
  Errors: number
  NoLevel: boolean // no row carried a log level, so Errors is unknown, not zero
  Min: number
  Avg: number
  P50: number
  P99: number
  Max: number
  SelfTotal: number // nanoseconds
}

export function parseTraceRows(lines: string[]): TraceRow[] {
  const raw = JSON.parse(window.wasmParseTraceRows(lines)) as TraceRow[] | { error: string }
  if (!Array.isArray(raw)) {
    throw new Error(raw.error || 'wasmParseTraceRows failed')
  }
  return raw
}

export function renderSpanRollup(lines: string[]): SpanRollupRow[] {
  const raw = JSON.parse(window.wasmRenderSpanRollup(lines)) as SpanRollupRow[] | null | { error: string }
  if (raw == null) return []
  if (!Array.isArray(raw)) {
    throw new Error((raw as { error: string }).error || 'wasmRenderSpanRollup failed')
  }
  return raw
}

function parseRows<T>(json: string, what: string): T[] {
  const raw = JSON.parse(json) as T[] | null | { error: string }
  if (raw == null) return []
  if (!Array.isArray(raw)) {
    throw new Error((raw as { error: string }).error || `${what} failed`)
  }
  return raw
}

export function renderSpanAggregate(lines: string[]): SpanRollupRow[] {
  return parseRows<SpanRollupRow>(window.wasmRenderSpanAggregate(lines), 'wasmRenderSpanAggregate')
}

export function renderSpanSamples(lines: string[]): SpanOccurrence[] {
  return parseRows<SpanOccurrence>(window.wasmRenderSpanSamples(lines), 'wasmRenderSpanSamples')
}

export function parseTraceList(lines: string[], nowMs: number = Date.now()): TraceListRow[] {
  return parseRows<TraceListRow>(window.wasmParseTraceList(lines, nowMs), 'wasmParseTraceList')
}

// The pipeline that lists one aggregate row's slowest occurrences, and the one
// that shows a single request. Built in Go (tracecmd) so the grammar and the
// quoting live in one place.
export function samplesPipeline(aggregate: string, name: string, label: string): string {
  return window.wasmSamplesPipeline(aggregate, name, label)
}

export function traceIDPipeline(id: string): string {
  return window.wasmTraceIDPipeline(id)
}
