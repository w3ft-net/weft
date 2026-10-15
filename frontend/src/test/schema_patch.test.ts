import { describe, expect, it } from 'vitest'
import { applyPatch, describeValue, mergePatches } from '../lib/schemaPatch'
import { suggestFromDetection, type ConfigureSourceRequest, type FieldCandidate, type SourceDetection } from '../lib/api'

const form = (over: Partial<ConfigureSourceRequest> = {}): ConfigureSourceRequest => ({
  format: 'text', timestamp_field: '', timestamp_format: '', level_field: '', level_pattern: '',
  trace_id_field: '', trace_id_pattern: '', span_name_field: '', span_name_pattern: '',
  span_id_field: '', span_id_pattern: '', parent_span_id_field: '', parent_span_id_pattern: '',
  duration_ns_field: '', duration_ns_pattern: '', ...over,
})

describe('schema patches', () => {
  it('merges each value\'s extract spec instead of replacing the whole map', () => {
    const a = { level_field: '3', extract: { level: { cut_at: '/' } } }
    const b = { trace_id_field: '-1', extract: { trace_id: { prefix: 'trace_id=' } } }
    expect(mergePatches(a, b).extract).toEqual({ level: { cut_at: '/' }, trace_id: { prefix: 'trace_id=' } })
  })

  it('applies a patch to the form, adding and clearing specs', () => {
    const f = form({ extract: { level: { cut_at: '/' }, trace_id: { prefix: 'x' } } })
    const next = applyPatch(f, { level_field: '', level_pattern: '', extract: { level: null, timestamp: { prefix: '[' } } })
    expect(next.level_field).toBe('')
    expect(next.extract).toEqual({ trace_id: { prefix: 'x' }, timestamp: { prefix: '[' } })
    expect(f.extract?.level).toEqual({ cut_at: '/' }) // the original is untouched
  })

  it('leaves the extract map alone when the patch has none', () => {
    const f = form({ extract: { level: { cut_at: '/' } } })
    expect(applyPatch(f, { level_field: '4' }).extract).toEqual({ level: { cut_at: '/' } })
  })
})

// The same sentences the Go side (ProbeExtractor.Describe) produces.
describe('describeValue', () => {
  it('says what is stripped', () => {
    expect(describeValue(form({ level_field: '3', extract: { level: { cut_at: '/' } } }), 'level'))
      .toBe('field 3, cut at "/"')
    expect(describeValue(form({ trace_id_field: '-1', extract: { trace_id: { prefix: 'trace_id=' } } }), 'trace_id'))
      .toBe('last field, without the leading "trace_id="')
    expect(describeValue(form({
      timestamp_field: '1', timestamp_fields: 2, timestamp_format: '2006-01-02 15:04:05',
      extract: { timestamp: { prefix: '[', suffix: ':' } },
    }), 'timestamp')).toBe('fields 1-2, without the leading "[" and the trailing ":", format 2006-01-02 15:04:05')
    expect(describeValue(form({ level_field: '-2', extract: { level: {} } }), 'level')).toBe('second-to-last field')
  })

  it('still describes the legacy forms', () => {
    expect(describeValue(form(), 'level')).toBe('not extracted')
    expect(describeValue(form({ trace_id_field: 'none', trace_id_pattern: 'id=(\\w+)' }), 'trace_id'))
      .toBe('anywhere in the line, matching /id=(\\w+)/')
    expect(describeValue(form({ level_field: '.level' }), 'level')).toBe('JSON field .level')
  })
})

describe('suggestFromDetection', () => {
  const det = (over: Partial<SourceDetection>): SourceDetection => ({
    Lines: null, JSONCoverage: 0, JSONTailCoverage: 0, JSONTailStartField: 0,
    TimestampCandidates: [{ FieldIndex: 1, SpanFields: 2, Format: '2006-01-02 15:04:05', JSONPath: '', Sample: 'x' }],
    LevelCandidates: [{ FieldIndex: 3, JSONPath: '', Sample: 'INFO', Pattern: '', CutAt: '/' } as FieldCandidate],
    TraceIDCandidates: null, SpanNameCandidates: null, SpanIDCandidates: null,
    ParentSpanIDCandidates: null, DurationNsCandidates: null, ...over,
  } as SourceDetection)

  // How to read each value is the wizard's to decide, from the server, which
  // learns it from the data; the form is not pre-filled with a client-side
  // reading of detection (which would need defaults of its own).
  it('pre-fills only the format, never how a value is read', () => {
    const s = suggestFromDetection(det({}))
    expect(s.format).toBe('text')
    expect(s.level_field).toBe('')
    expect(s.timestamp_field).toBe('')
    expect(s.extract).toBeUndefined()
  })

  it('picks the format from how much of the sample is JSON', () => {
    expect(suggestFromDetection(det({ JSONCoverage: 0.95 })).format).toBe('json')
    const tail = suggestFromDetection(det({ JSONTailCoverage: 0.8, JSONTailStartField: 4 }))
    expect(tail.format).toBe('prefix_json')
    expect(tail.json_start_field).toBe(4)
  })
})
