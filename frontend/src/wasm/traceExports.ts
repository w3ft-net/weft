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
  }
}

export interface TraceRow {
  Raw: string
  IsData: boolean
  TsNs: number
  Label: string
  Message: string
  GapNs: number
  ColorIdx: number
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
  Occurrences: SpanOccurrence[] | null
  Errors: number
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
