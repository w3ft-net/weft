import type { ConfigureSourceRequest, ExtractSpec } from './api'

// A schema patch is what the server's probe says to change in the configure
// form for one value (POST …/probe -> schema_patch): flat field/pattern keys,
// plus `extract`, a map from value name to its spec (null clears it).
export type SchemaPatch = Record<string, string | number | Record<string, ExtractSpec | null> | undefined>

type ExtractPatch = Record<string, ExtractSpec | null>

// mergePatches combines two patches. Flat keys are overwritten by the later
// one; `extract` maps are merged per value, so one value's spec never wipes
// out another's.
export function mergePatches(a: SchemaPatch, b: SchemaPatch): SchemaPatch {
  const out: SchemaPatch = { ...a, ...b }
  if (a.extract || b.extract) {
    out.extract = { ...(a.extract as ExtractPatch | undefined), ...(b.extract as ExtractPatch | undefined) }
  }
  return out
}

// applyPatch returns the form with the patch applied; the input is untouched.
// A null spec removes that value's entry from the form's extract map.
export function applyPatch(form: ConfigureSourceRequest, patch: SchemaPatch): ConfigureSourceRequest {
  const { extract, ...flat } = patch
  const next = { ...form, ...flat } as ConfigureSourceRequest
  if (extract) {
    const merged: Record<string, ExtractSpec> = { ...(form.extract ?? {}) }
    for (const [name, spec] of Object.entries(extract as ExtractPatch)) {
      if (spec === null) delete merged[name]
      else merged[name] = spec
    }
    next.extract = merged
  }
  return next
}

function ordinalFromEnd(n: number, span: number): string {
  if (span === 1 && n === -1) return 'last field'
  if (span === 1 && n === -2) return 'second-to-last field'
  if (span > 1) return `${span} fields starting ${-n} from the end`
  return `field ${-n} from the end`
}

// describeValue puts one value's extraction rule in words. It mirrors
// ProbeExtractor.Describe in ingest/probe.go (the tests share its sentences).
export function describeValue(form: ConfigureSourceRequest, key: string): string {
  const rec = form as unknown as Record<string, string | number | undefined>
  const field = String(rec[`${key}_field`] ?? '')
  const pattern = key === 'timestamp' ? '' : String(rec[`${key}_pattern`] ?? '')
  const spec = form.extract?.[key]
  const isTS = key === 'timestamp'

  if (isTS && (!field || field === 'none')) return 'ingestion time'
  if (!field && !pattern) return 'not extracted'

  const tail =
    (spec?.fallback ? `, else /${spec.fallback}/` : '') +
    (isTS && form.timestamp_format ? `, format ${form.timestamp_format}` : '')

  if (field === 'none' || !field) return `anywhere in the line, matching /${pattern}/${tail}`
  if (field.startsWith('.')) return `JSON field ${field}${tail}`

  const n = Number(field)
  if (!Number.isFinite(n)) return `field ${field}${tail}`
  const span = isTS ? Math.max(form.timestamp_fields ?? 1, 1) : 1
  const parts: string[] = []
  if (n > 0) parts.push(span > 1 ? `fields ${n}-${n + span - 1}` : `field ${n}`)
  else parts.push(ordinalFromEnd(n, span))

  if (spec) {
    const strip: string[] = []
    if (spec.prefix) strip.push(`the leading ${JSON.stringify(spec.prefix)}`)
    if (spec.suffix) strip.push(`the trailing ${JSON.stringify(spec.suffix)}`)
    if (strip.length) parts.push(`without ${strip.join(' and ')}`)
    if (spec.chars) parts.push(`trimming ${JSON.stringify(spec.chars)} from both ends`)
    if (spec.cut_at) parts.push(`cut at ${JSON.stringify(spec.cut_at)}`)
  }
  if (pattern) parts.push(`matching /${pattern}/`)
  return parts.join(', ') + tail
}
