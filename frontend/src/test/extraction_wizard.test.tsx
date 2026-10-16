// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'

import ExtractionWizard from '../components/ExtractionWizard'
import type { ProbeFn, ProbeRequest, ProbeResult } from '../lib/api'

const sample = [
  '12:00:00 starting worker',
  '12:00:02 GET /api/items 200 trace_id=3f9a1c0de4b2a871',
  '12:00:03 GET /healthz 200',
  '12:00:04 GET /api/orders 500 trace_id=aa01bb02cc03dd04',
]
const base = { envelope: '', format: 'text', field_separator: '', json_start_field: 0 }

// A stand-in for the server: finds `trace_id=(\w+)` on the sample, honoring
// "none" marks, and reports the way POST …/probe does.
function fakeProbe(opts: { detectedTimestamp?: boolean } = {}): { probe: ProbeFn; calls: ProbeRequest[] } {
  const calls: ProbeRequest[] = []
  const probe: ProbeFn = async req => {
    calls.push(req)
    const empty: ProbeResult = { extractor: null, results: [], matched: 0, total: sample.length }
    if (req.field === 'timestamp' && req.use_detected && opts.detectedTimestamp) {
      return {
        extractor: { field: '1', pattern: '', timestamp_format: '15:04:05', timestamp_fields: 1 },
        results: sample.map((l, i) => ({ line: i, matched: true, value: 'x', text: l.slice(0, 8), prefix: '' })),
        matched: sample.length, total: sample.length,
        schema_patch: { timestamp_field: '1', timestamp_format: '15:04:05', timestamp_fields: 1 },
      }
    }
    if (req.field !== 'trace_id' || req.marks.length === 0) return empty
    const none = new Set(req.marks.filter(m => m.none).map(m => m.line))
    const re = /trace_id=(\w+)/
    const results = sample.map((l, i) => {
      const m = re.exec(l)
      if (!m || none.has(i)) return { line: i, matched: false, value: '' }
      const at = m.index + 'trace_id='.length
      return { line: i, matched: true, value: m[1], text: m[1], prefix: l.slice(0, at) }
    })
    return {
      extractor: { field: 'none', pattern: 'trace_id=(\\w+)' },
      results, matched: results.filter(r => r.matched).length, total: sample.length,
      schema_patch: { trace_id_field: 'none', trace_id_pattern: 'trace_id=(\\w+)' },
    }
  }
  return { probe, calls }
}

function selectIn(testid: string, text: string) {
  const el = screen.getByTestId(testid)
  const node = el.firstChild as Text
  const i = node.data.indexOf(text)
  const range = document.createRange()
  range.setStart(node, i)
  range.setEnd(node, i + text.length)
  const sel = window.getSelection()!
  sel.removeAllRanges()
  sel.addRange(range)
  fireEvent.mouseUp(el)
}

const T = 'p-wizard'
const click = (id: string) => fireEvent.click(screen.getByTestId(`${T}-${id}`))
const see = (id: string) => screen.findByTestId(`${T}-${id}`)
// Wait for a control to appear, then click it.
async function press(id: string) {
  await see(id)
  click(id)
}

describe('ExtractionWizard decision tree', () => {
  afterEach(cleanup)

  it('accepts detection, skips what is not expected, and finds a trace id by highlighting', async () => {
    const { probe, calls } = fakeProbe({ detectedTimestamp: true })
    const done = vi.fn()
    render(<ExtractionWizard testidPrefix="p" sample={sample} base={base} probe={probe}
      spansDetected={false} onComplete={done} onCancel={() => {}} />)

    // timestamp: detection's guess, accepted
    await see('confirm-yes-button')
    expect(calls[0]).toMatchObject({ field: 'timestamp', use_detected: true })
    click('confirm-yes-button')

    // level: nothing detected, not expected -> skipped
    await press('expect-no-button')

    // trace id: expected; not on lines 0, then highlight it on line 1
    await press('expect-yes-button')
    await see('pick-line')
    expect(screen.getByTestId(`${T}-pick-line`).textContent).toBe(sample[0])
    click('next-line-button')
    await waitFor(() => expect(screen.getByTestId(`${T}-pick-line`).textContent).toBe(sample[1]))
    selectIn(`${T}-pick-line`, '3f9a1c0de4b2a871')
    click('use-selection-button')

    // the rule was built from the highlight on line 1, with its prefix
    await see('rows')
    const mark = calls[calls.length - 1].marks[0]
    expect(mark).toEqual({ line: 1, prefix: '12:00:02 GET /api/items 200 trace_id=', text: '3f9a1c0de4b2a871', none: false })

    // review: the healthz line has no value (fine), line 3 matched; flag line 3 wrong
    expect(screen.getByTestId(`${T}-row-3`).querySelector('mark')?.textContent).toBe('aa01bb02cc03dd04')
    click('wrong-3-button')
    await waitFor(() => {
      const last = calls[calls.length - 1]
      expect(last.marks).toContainEqual({ line: 3, prefix: '', text: '', none: true })
    })
    await waitFor(() => expect(screen.queryByTestId(`${T}-wrong-3-button`)).toBeNull())
    click('accept-button')

    // spans gate (none detected): decline
    await press('spans-no-button')

    expect(done).toHaveBeenCalledTimes(1)
    const [patch, notes] = done.mock.calls[0]
    expect(patch).toMatchObject({
      timestamp_field: '1', timestamp_format: '15:04:05', timestamp_fields: 1,
      level_field: '', level_pattern: '',
      trace_id_field: 'none', trace_id_pattern: 'trace_id=(\\w+)',
    })
    expect(notes).toEqual([])
  })

  it('reports a value that is on none of the lines and moves on', async () => {
    const { probe } = fakeProbe()
    const done = vi.fn()
    render(<ExtractionWizard testidPrefix="p" sample={['a', 'b']} base={base} probe={probe}
      spansDetected={true} onComplete={done} onCancel={() => {}} />)

    // timestamp, level: not expected
    await press('expect-no-button')
    await waitFor(() => expect(screen.getByTestId(`${T}-header`).textContent).toContain('log level'))
    await press('expect-no-button')
    // trace: expected, but neither line has it
    await waitFor(() => expect(screen.getByTestId(`${T}-header`).textContent).toContain('trace ID'))
    click('expect-yes-button')
    await see('pick-line')
    click('next-line-button')
    await waitFor(() => expect(screen.getByTestId(`${T}-pick-line`).textContent).toBe('b'))
    click('next-line-button')

    // spans were detected, so they are walked rather than gated; skip all four
    for (let i = 0; i < 4; i++) {
      await waitFor(() => expect(screen.queryByTestId(`${T}-expect-no-button`)).not.toBeNull())
      click('expect-no-button')
    }
    await waitFor(() => expect(done).toHaveBeenCalled())
    const [patch, notes] = done.mock.calls[0]
    expect(patch).toMatchObject({ trace_id_field: '', trace_id_pattern: '' })
    expect(notes).toHaveLength(1)
    expect(notes[0]).toContain('trace ID')
    expect(notes[0]).toContain('fresher sample')
  })

  it('refuses a selection no rule fits and keeps asking', async () => {
    const probe: ProbeFn = async req => req.marks.length === 0
      ? { extractor: null, results: [], matched: 0, total: 1 }
      : { extractor: null, results: [], matched: 0, total: 1, message: 'no extraction rule matches every line you marked' }
    render(<ExtractionWizard testidPrefix="p" sample={['id=abc ok']} base={base} probe={probe}
      spansDetected={true} onComplete={() => {}} onCancel={() => {}} />)
    await press('expect-yes-button')
    await see('pick-line')
    selectIn(`${T}-pick-line`, 'abc')
    click('use-selection-button')
    expect((await see('notice')).textContent).toContain('no extraction rule matches')
    expect(screen.getByTestId(`${T}-pick-line`)).toBeTruthy()
  })

  it('says so when the model proposed the rule', async () => {
    const probe: ProbeFn = async req => req.marks.length === 0
      ? { extractor: null, results: [], matched: 0, total: 1 }
      : {
          extractor: { field: 'none', pattern: 'id=(\\w+)' },
          source: 'model',
          results: [{ line: 0, matched: true, value: 'abc', text: 'abc', prefix: 'id=' }],
          matched: 1, total: 1,
          schema_patch: { trace_id_field: 'none', trace_id_pattern: 'id=(\\w+)' },
        }
    render(<ExtractionWizard testidPrefix="p" sample={['id=abc ok']} base={base} probe={probe}
      spansDetected={true} onComplete={() => {}} onCancel={() => {}} />)
    await press('expect-yes-button')
    await see('pick-line')
    selectIn(`${T}-pick-line`, 'abc')
    click('use-selection-button')
    expect((await see('model-note')).textContent).toContain('model proposed')
  })

  it('offers each detected candidate in turn, showing the values it sees', async () => {
    const asked: Array<number | undefined> = []
    const probe: ProbeFn = async req => {
      if (req.field !== 'level' || !req.use_detected) return { extractor: null, results: [], matched: 0, total: 2 }
      asked.push(req.detected_index)
      const cands = [
        { field: '1', values: ['GET', 'POST'] },
        { field: '2', values: ['INFO', 'WARN'] },
      ]
      const c = cands[req.detected_index ?? 0]
      if (!c) return { extractor: null, results: [], matched: 0, total: 2, detected_total: 2 }
      return {
        extractor: { field: c.field, pattern: '', spec: {} },
        results: c.values.map((v, i) => ({ line: i, matched: true, value: v, text: v, prefix: '' })),
        matched: 2, total: 2, detected_total: 2,
        schema_patch: { level_field: c.field, level_pattern: '', extract: { level: {} } },
      }
    }
    const done = vi.fn()
    render(<ExtractionWizard testidPrefix="p" sample={['GET INFO a', 'POST WARN b']} base={base} probe={probe}
      spansDetected={true} onComplete={done} onCancel={() => {}} />)

    await press('expect-no-button') // timestamp: nothing detected, not expected
    // level: first candidate (the method field) is shown with what it sees
    expect((await see('candidate-values')).textContent).toContain('GET, POST')
    click('confirm-no-button')
    // ...then the second
    await waitFor(() => expect(screen.getByTestId(`${T}-candidate-values`).textContent).toContain('INFO, WARN'))
    expect(asked).toEqual([0, 1])
    click('confirm-yes-button')

    // the accepted candidate is the one that lands in the schema
    for (let i = 0; i < 5; i++) { await press('expect-no-button') } // trace ID and the four span values, all skipped
    await waitFor(() => expect(done).toHaveBeenCalled())
    expect(done.mock.calls[0][0]).toMatchObject({ level_field: '2' })
  })

  it('offers the model\'s first guess when detection has nothing, and asks the unit of a bare duration', async () => {
    const lines = ['12:00:00 start', '12:00:02 GET /a 200 took=12.5', '12:00:04 GET /b 500 took=340']
    const reqs: ProbeRequest[] = []
    const probe: ProbeFn = async req => {
      reqs.push(req)
      const empty: ProbeResult = { extractor: null, results: [], matched: 0, total: lines.length }
      if (req.field === 'timestamp') {
        return req.propose
          ? {
              extractor: { field: '1', pattern: '', timestamp_format: '15:04:05', timestamp_fields: 1 },
              source: 'model',
              results: lines.map((l, i) => ({ line: i, matched: true, value: 'x', text: l.slice(0, 8), prefix: '' })),
              matched: 3, total: 3,
              schema_patch: { timestamp_field: '1', timestamp_format: '15:04:05', timestamp_fields: 1 },
            }
          : empty
      }
      if (req.field !== 'duration_ns' || req.marks.length === 0) return empty
      const results = lines.map((l, i) => {
        const m = /took=([\d.]+)/.exec(l)
        if (!m) return { line: i, matched: false, value: '' }
        return { line: i, matched: true, value: m[1], text: m[1], prefix: l.slice(0, m.index + 5) }
      })
      return {
        extractor: { field: 'none', pattern: 'took=([\\d.]+)', duration_unit: req.duration_unit ?? '' },
        results, matched: 2, total: 3,
        needs_unit: !req.duration_unit,
        schema_patch: { duration_ns_field: 'none', duration_ns_pattern: 'took=([\\d.]+)', duration_unit: req.duration_unit ?? '' },
      }
    }
    const done = vi.fn()
    render(<ExtractionWizard testidPrefix="p" sample={lines} base={base} probe={probe}
      spansDetected={true} onComplete={done} onCancel={() => {}} />)

    // timestamp: nothing detected, so the model's guess is shown and accepted
    await see('confirm-yes-button')
    expect(reqs.some(r => r.field === 'timestamp' && r.propose)).toBe(true)
    click('confirm-yes-button')
    for (let i = 0; i < 4; i++) { await press('expect-no-button') } // level, trace, span name/id... skipped
    await press('expect-no-button') // parent span id
    // duration: highlight a bare number -> the wizard asks what it counts
    await press('expect-yes-button')
    await see('pick-line')
    click('next-line-button')
    await waitFor(() => expect(screen.getByTestId(`${T}-pick-line`).textContent).toBe(lines[1]))
    selectIn(`${T}-pick-line`, '12.5')
    click('use-selection-button')
    await press('unit-ms-button')
    await see('rows')
    expect(reqs[reqs.length - 1].duration_unit).toBe('ms')
    click('accept-button')
    await waitFor(() => expect(done).toHaveBeenCalled())
    expect(done.mock.calls[0][0]).toMatchObject({ duration_ns_pattern: 'took=([\\d.]+)', duration_unit: 'ms' })
  })
})
