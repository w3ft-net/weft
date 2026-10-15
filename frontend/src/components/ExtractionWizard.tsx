import { useEffect, useRef, useState } from 'react'
import type { ProbeExtractor, ProbeFn, ProbeField, ProbeLineResult, ProbeMark, ProbeResult } from '../lib/api'
import { mergePatches, type SchemaPatch } from '../lib/schemaPatch'

// ExtractionWizard walks every extractable value (timestamp, level, trace ID,
// and optionally the span fields) through the same decision tree as the CLI
// wizard (cmd/weft/setup/tree.go):
//
//   1. show detection's guess, highlighted on the sample; accept it, or
//   2. "do you expect this value on lines like these?" no -> skip it
//   3. yes -> show lines one at a time until the user highlights the value
//   4. the server builds a rule from the highlight and applies it to the whole
//      sample; the user flags lines it missed or got wrong; repeat until accepted
//
// All rule-building happens server-side (POST …/probe); this component only
// holds the conversation. onComplete gets the schema fields to merge into the
// configure request.

interface FieldDef { key: ProbeField; label: string }

const FIELDS: FieldDef[] = [
  { key: 'timestamp', label: 'timestamp' },
  { key: 'level', label: 'log level' },
  { key: 'trace_id', label: 'trace ID' },
  { key: 'span_name', label: 'span name' },
  { key: 'span_id', label: 'span ID' },
  { key: 'parent_span_id', label: 'parent span ID' },
  { key: 'duration_ns', label: 'span duration' },
]
const TRACE_IDX = 2 // spans are gated after the trace ID

type Stage = 'loading' | 'confirm' | 'expect' | 'pick' | 'review' | 'spans-gate'
type Patch = SchemaPatch

// What clearing a field looks like in the configure request.
function clearPatch(f: ProbeField): Patch {
  const extract = { [f]: null }
  if (f === 'timestamp') return { timestamp_field: '', timestamp_format: '', timestamp_fields: 0, extract }
  return { [`${f}_field`]: '', [`${f}_pattern`]: '', extract }
}

function setMark(marks: ProbeMark[], m: ProbeMark): ProbeMark[] {
  const i = marks.findIndex(x => x.line === m.line)
  if (i < 0) return [...marks, m]
  const next = [...marks]
  next[i] = m
  return next
}

// HighlightedLine draws a sample line with the extracted value marked.
// The text content of the element is always exactly the line, so character
// offsets from a browser selection match the line's own.
export function HighlightedLine({ line, result }: { line: string; result?: ProbeLineResult }) {
  const prefix = result?.prefix
  const text = result?.text
  if (result?.matched && text && prefix !== undefined && line.startsWith(prefix) && line.startsWith(text, prefix.length)) {
    return (
      <>
        {prefix}
        <mark className="psc-hit">{text}</mark>
        {line.slice(prefix.length + text.length)}
      </>
    )
  }
  return <>{line}</>
}

// SelectableLine lets the user drag-select (or double-click) the value in a
// line, and reports it as the line's text before it plus the text itself.
function SelectableLine({
  line, testid, onSelect,
}: {
  line: string
  testid: string
  onSelect: (sel: { prefix: string; text: string } | null) => void
}) {
  const ref = useRef<HTMLDivElement>(null)

  function read() {
    const el = ref.current
    const sel = window.getSelection()
    if (!el || !sel || sel.rangeCount === 0 || sel.isCollapsed) { onSelect(null); return }
    const range = sel.getRangeAt(0)
    if (!el.contains(range.startContainer) || !el.contains(range.endContainer)) { onSelect(null); return }
    const pre = range.cloneRange()
    pre.selectNodeContents(el)
    pre.setEnd(range.startContainer, range.startOffset)
    let start = pre.toString().length
    let text = range.toString()
    const lead = text.length - text.trimStart().length
    start += lead
    text = text.trim()
    onSelect(text ? { prefix: line.slice(0, start), text } : null)
  }

  return (
    <div
      ref={ref}
      className="psc-wiz-line psc-wiz-selectable"
      onMouseUp={read}
      onKeyUp={read}
      data-testid={testid}
    >
      {line}
    </div>
  )
}

export default function ExtractionWizard({
  testidPrefix, sample, base, probe, spansDetected, onComplete, onCancel,
}: {
  testidPrefix: string
  sample: string[]
  base: { envelope: string; format: string; field_separator: string; json_start_field: number }
  probe: ProbeFn
  spansDetected: boolean
  onComplete: (patch: Patch, notes: string[]) => void
  onCancel: () => void
}) {
  const tp = `${testidPrefix}-wizard`
  const [fi, setFi] = useState(0)
  const [stage, setStage] = useState<Stage>('loading')
  const [marks, setMarks] = useState<ProbeMark[]>([])
  const [prev, setPrev] = useState<ProbeExtractor | null>(null)
  const [result, setResult] = useState<ProbeResult | null>(null)
  const [line, setLine] = useState(0)
  const [seen, setSeen] = useState<Set<number>>(new Set([0]))
  const [flagLine, setFlagLine] = useState<number | null>(null)
  const [selection, setSelection] = useState<{ prefix: string; text: string } | null>(null)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [cand, setCand] = useState({ idx: 0, total: 0 })
  const patches = useRef<Patch>({})
  const notes = useRef<string[]>([])
  const field = FIELDS[fi]

  function run(f: ProbeField, m: ProbeMark[], previous: ProbeExtractor | null, detectedIndex?: number) {
    return probe({
      field: f, base, marks: m,
      previous: previous ?? undefined,
      use_detected: detectedIndex !== undefined ? true : undefined,
      detected_index: detectedIndex,
    })
  }

  function resetField() {
    setMarks([]); setPrev(null); setResult(null); setLine(0); setSeen(new Set([0]))
    setFlagLine(null); setSelection(null); setNotice(''); setError('')
  }

  // Offer detected candidate idx of field i, or, when there is none left,
  // ask whether the value is expected at all.
  async function offerCandidate(i: number, idx: number) {
    setStage('loading')
    try {
      const res = await run(FIELDS[i].key, [], null, idx)
      if (res.extractor) {
        setResult(res)
        setCand({ idx, total: res.detected_total ?? idx + 1 })
        setStage('confirm')
        return
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
    setResult(null)
    setStage('expect')
  }

  // Enter field i: show the first detected candidate, if there is one.
  async function enterField(i: number) {
    resetField()
    setFi(i)
    await offerCandidate(i, 0)
  }

  useEffect(() => {
    enterField(0)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Record this field's answer and move on.
  function advance(patch: Patch) {
    patches.current = mergePatches(patches.current, patch)
    if (fi === FIELDS.length - 1) { onComplete(patches.current, notes.current); return }
    if (fi === TRACE_IDX && !spansDetected) { resetField(); setStage('spans-gate'); return }
    enterField(fi + 1)
  }

  function skipField() {
    advance(clearPatch(field.key))
  }

  async function guarded(fn: () => Promise<void>) {
    setBusy(true)
    setError('')
    try { await fn() } catch (e) { setError(e instanceof Error ? e.message : String(e)) } finally { setBusy(false) }
  }

  function nextLine(delta: 1 | -1) {
    if (delta === 1 && seen.size >= sample.length) {
      notes.current.push(`None of the sample lines has a ${field.label}. If it appears only on some requests, collect a fresher sample.`)
      skipField()
      return
    }
    const n = (line + delta + sample.length) % sample.length
    setLine(n)
    setSeen(new Set([...seen, n]))
    setSelection(null)
  }

  function useSelection() {
    if (!selection) return
    guarded(async () => {
      const target = flagLine ?? line
      const next = setMark(marks, { line: target, prefix: selection.prefix, text: selection.text, none: false })
      const res = await run(field.key, next, prev)
      if (!res.extractor) {
        setNotice(res.message ?? 'No rule matches that selection.')
        setSelection(null)
        return
      }
      setMarks(next); setPrev(res.extractor); setResult(res)
      setFlagLine(null); setSelection(null); setNotice('')
      setStage('review')
    })
  }

  function flagWrong(i: number) {
    guarded(async () => {
      const next = setMark(marks, { line: i, prefix: '', text: '', none: true })
      const res = await run(field.key, next, prev)
      if (!res.extractor) { setNotice(res.message ?? 'No rule fits those lines.'); return }
      setMarks(next); setPrev(res.extractor); setResult(res); setNotice('')
    })
  }

  function flagMissing(i: number) {
    setFlagLine(i); setLine(i); setSelection(null); setStage('pick')
  }

  function restart() {
    resetField()
    setStage('pick')
  }

  const header = (
    <div className="psc-wiz-head" data-testid={`${tp}-header`}>
      <span className="tui-tag tui-tag-hi">{fi + 1}/{FIELDS.length}</span>{' '}
      <strong>{field.label}</strong>
      <button className="psc-textbtn" onClick={onCancel} data-testid={`${tp}-cancel-button`}>
        cancel
      </button>
    </div>
  )

  const messages = (
    <>
      {notice && <div className="psc-wiz-notice" data-testid={`${tp}-notice`}>{notice}</div>}
      {error && <div className="psc-error" data-testid={`${tp}-error`}>error: {error}</div>}
    </>
  )

  if (stage === 'loading') {
    return <div className="psc-wizard" data-testid={tp}>{header}<p className="psc-prose">looking…</p>{messages}</div>
  }

  if (stage === 'spans-gate') {
    return (
      <div className="psc-wizard" data-testid={tp}>
        <div className="psc-wiz-head"><strong>spans</strong></div>
        <p className="psc-prose">
          Does this service log spans (span name, ID, parent, duration)? Only if it uses weft-go
          or something like it. Say no if you’re unsure.
        </p>
        <div className="psc-actions">
          <button className="tui-btn tui-btn-primary" onClick={() => enterField(TRACE_IDX + 1)}
            data-testid={`${tp}-spans-yes-button`}>yes, walk through them</button>
          <button className="tui-btn" onClick={() => onComplete(patches.current, notes.current)}
            data-testid={`${tp}-spans-no-button`}>no</button>
        </div>
      </div>
    )
  }

  if (stage === 'confirm' && result) {
    const shown = result.results.filter(r => r.matched).slice(0, 5)
    const values = [...new Set(result.results.filter(r => r.matched).map(r => r.value))].slice(0, 8)
    return (
      <div className="psc-wizard" data-testid={tp}>
        {header}
        <p className="psc-prose">
          Found a {field.label} on <strong>{result.matched} of {result.total}</strong> lines
          {cand.total > 1 ? <> (candidate {cand.idx + 1} of {cand.total})</> : null}. Is this it?
        </p>
        <div className="psc-wiz-rule" data-testid={`${tp}-candidate-values`}>
          sees: {values.join(', ')}
        </div>
        <pre className="psc-sample" data-testid={`${tp}-candidate`}>
          {shown.map(r => (
            <div key={r.line} className="psc-sample-line">
              <HighlightedLine line={sample[r.line]} result={r} />
            </div>
          ))}
        </pre>
        <div className="psc-actions">
          <button className="tui-btn tui-btn-primary" disabled={busy}
            onClick={() => advance(result.schema_patch ?? {})}
            data-testid={`${tp}-confirm-yes-button`}>yes, that’s the {field.label}</button>
          <button className="tui-btn" onClick={() => offerCandidate(fi, cand.idx + 1)}
            data-testid={`${tp}-confirm-no-button`}>no{cand.idx + 1 < cand.total ? ', show the next' : ''}</button>
        </div>
        {messages}
      </div>
    )
  }

  if (stage === 'expect') {
    return (
      <div className="psc-wizard" data-testid={tp}>
        {header}
        <p className="psc-prose">Do you expect a {field.label} on lines like these?</p>
        <div className="psc-actions">
          <button className="tui-btn tui-btn-primary" onClick={() => { resetField(); setStage('pick') }}
            data-testid={`${tp}-expect-yes-button`}>yes, help me find it</button>
          <button className="tui-btn" onClick={skipField}
            data-testid={`${tp}-expect-no-button`}>no, skip it</button>
        </div>
        {messages}
      </div>
    )
  }

  if (stage === 'pick') {
    const target = flagLine ?? line
    return (
      <div className="psc-wizard" data-testid={tp}>
        {header}
        <p className="psc-prose">
          {flagLine !== null
            ? <>The rule missed line {target + 1}. Select the {field.label} on it.</>
            : <>Is the {field.label} on this line? Select it (drag, or double-click a word).</>}
        </p>
        <div className="psc-sample-label">line {target + 1}/{sample.length}</div>
        <SelectableLine
          line={sample[target]}
          testid={`${tp}-pick-line`}
          onSelect={setSelection}
        />
        <div className="psc-actions">
          <button className="tui-btn tui-btn-primary" disabled={!selection || busy} onClick={useSelection}
            data-testid={`${tp}-use-selection-button`}>
            {selection ? <>use “{selection.text}”</> : 'select the value first'}
          </button>
          {flagLine === null ? (
            <>
              <button className="tui-btn" onClick={() => nextLine(1)}
                data-testid={`${tp}-next-line-button`}>not on this line, next</button>
              <button className="tui-btn" onClick={() => nextLine(-1)}
                data-testid={`${tp}-prev-line-button`}>previous line</button>
            </>
          ) : (
            <button className="tui-btn" onClick={() => { setFlagLine(null); setStage('review') }}
              data-testid={`${tp}-cancel-flag-button`}>back to review</button>
          )}
          <button className="tui-btn" onClick={skipField}
            data-testid={`${tp}-skip-button`}>skip this field</button>
        </div>
        {messages}
      </div>
    )
  }

  // review
  return (
    <div className="psc-wizard" data-testid={tp}>
      {header}
      <p className="psc-prose">
        This rule finds a {field.label} on <strong>{result?.matched ?? 0} of {result?.total ?? 0}</strong> lines.
        Flag any line it missed or got wrong.
      </p>
      {result?.source === 'model' && (
        <div className="psc-wiz-notice" data-testid={`${tp}-model-note`}>
          No simple rule fit what you marked, so the model proposed this one. Check it against the lines below.
        </div>
      )}
      {prev && (
        <div className="psc-wiz-rule" data-testid={`${tp}-rule`}>
          {result?.description ?? (prev.pattern ? `matching /${prev.pattern}/` : `field ${prev.field}`)}
        </div>
      )}
      <div className="psc-wiz-rows" data-testid={`${tp}-rows`}>
        {sample.map((l, i) => {
          const r = result?.results[i]
          const mk = marks.find(m => m.line === i)
          return (
            <div key={i} className={`psc-wiz-row${r?.matched ? ' psc-wiz-row-hit' : ''}`}
              data-testid={`${tp}-row-${i}`}>
              <span className="psc-wiz-mark">{mk ? (mk.none ? '✗' : '✓') : r?.matched ? '✓' : '·'}</span>
              <span className="psc-wiz-text"><HighlightedLine line={l} result={r} /></span>
              <span className="psc-wiz-flags">
                {!r?.matched && (
                  <button className="psc-textbtn" disabled={busy} onClick={() => flagMissing(i)}
                    data-testid={`${tp}-missed-${i}-button`}>missed</button>
                )}
                {r?.matched && (
                  <button className="psc-textbtn" disabled={busy} onClick={() => flagWrong(i)}
                    data-testid={`${tp}-wrong-${i}-button`}>wrong</button>
                )}
              </span>
            </div>
          )
        })}
      </div>
      <div className="psc-actions">
        <button className="tui-btn tui-btn-primary" disabled={busy}
          onClick={() => advance(result?.schema_patch ?? {})}
          data-testid={`${tp}-accept-button`}>looks right</button>
        <button className="tui-btn" onClick={restart}
          data-testid={`${tp}-restart-button`}>start over</button>
        <button className="tui-btn" onClick={skipField}
          data-testid={`${tp}-review-skip-button`}>skip this field</button>
      </div>
      {messages}
    </div>
  )
}
