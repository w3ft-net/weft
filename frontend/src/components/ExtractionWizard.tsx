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

type Stage = 'loading' | 'confirm' | 'expect' | 'pick' | 'review' | 'unit' | 'spans-gate'
type Patch = SchemaPatch

// What clearing a field looks like in the configure request.
function clearPatch(f: ProbeField): Patch {
  const extract = { [f]: null }
  if (f === 'timestamp') return { timestamp_field: '', timestamp_format: '', timestamp_fields: 0, extract }
  const patch: Patch = { [`${f}_field`]: '', [`${f}_pattern`]: '', extract }
  if (f === 'duration_ns') patch.duration_unit = ''
  return patch
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
  // The server omits an empty prefix: a value that starts the line has none.
  const prefix = result?.prefix ?? ''
  const text = result?.text
  if (result?.matched && text && line.startsWith(prefix) && line.startsWith(text, prefix.length)) {
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

const PREVIEW_LINES = 12

// SamplePreview is the sample as the CLI shows it at the start of each value:
// the lines themselves, with the value the current rule (or guess) finds
// highlighted in green. Without a result it is the plain sample, so the
// person is never looking at a blank panel while a guess is being fetched.
function SamplePreview({ sample, result, testid }: { sample: string[]; result?: ProbeResult | null; testid: string }) {
  const lines = sample.slice(0, PREVIEW_LINES)
  return (
    <>
      <pre className="psc-sample psc-wiz-preview" data-testid={testid}>
        {lines.map((l, i) => (
          <div key={i} className="psc-sample-line">
            <HighlightedLine line={l} result={result?.results[i]} />
          </div>
        ))}
      </pre>
      {sample.length > lines.length && (
        <div className="psc-sample-label">+{sample.length - lines.length} more lines</div>
      )}
    </>
  )
}

// isIDRune is what an identifier-like value (hex id, uuid, request id) is made
// of. It is the CLI picker's notion too (cmd/weft/setup/picker.go), so a
// double-click grabs the same token that `enter` does there.
const isIDRune = (c: string) => /[\p{L}\p{N}_.-]/u.test(c)

// SelectableLine lets the user drag-select (or double-click) the value in a
// line, and reports it as the line's text before it plus the text itself.
// Like the CLI picker, the selection is drawn by us (purple), not left to the
// browser's native highlight, so it stays visible while the buttons are used.
function SelectableLine({
  line, testid, selection, onSelect,
}: {
  line: string
  testid: string
  selection: { prefix: string; text: string } | null
  onSelect: (sel: { prefix: string; text: string } | null) => void
}) {
  const ref = useRef<HTMLDivElement>(null)

  function read(e: React.MouseEvent) {
    const el = ref.current
    const sel = window.getSelection()
    if (!el || !sel || sel.rangeCount === 0 || sel.isCollapsed) { return }
    const range = sel.getRangeAt(0)
    if (!el.contains(range.startContainer) || !el.contains(range.endContainer)) { onSelect(null); return }
    const pre = range.cloneRange()
    pre.selectNodeContents(el)
    pre.setEnd(range.startContainer, range.startOffset)
    let start = pre.toString().length
    let end = start + range.toString().length
    if (e.detail >= 2) {
      // Double-click: the whole identifier-like token, as `enter` takes in the CLI.
      while (start > 0 && isIDRune(line[start - 1])) start--
      while (end < line.length && isIDRune(line[end])) end++
    }
    while (start < end && /\s/.test(line[start])) start++
    while (end > start && /\s/.test(line[end - 1])) end--
    sel.removeAllRanges()
    onSelect(end > start ? { prefix: line.slice(0, start), text: line.slice(start, end) } : null)
  }

  const shown = selection && line.startsWith(selection.prefix) && line.startsWith(selection.text, selection.prefix.length)
  return (
    <div
      ref={ref}
      className="psc-wiz-line psc-wiz-selectable"
      onMouseUp={read}
      onDoubleClick={read}
      data-testid={testid}
    >
      {shown ? (
        <>
          {selection.prefix}
          <mark className="psc-sel" data-testid={`${testid}-selection`}>{selection.text}</mark>
          {line.slice(selection.prefix.length + selection.text.length)}
        </>
      ) : line}
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
  // The unit chosen for bare duration numbers, once asked.
  const durationUnit = useRef<string | null>(null)
  // Fields whose first model guess has already been offered.
  const proposed = useRef<Set<number>>(new Set())
  const [pendingUnit, setPendingUnit] = useState<{ marks: ProbeMark[]; result: ProbeResult } | null>(null)
  const notes = useRef<string[]>([])
  const field = FIELDS[fi]

  function run(f: ProbeField, m: ProbeMark[], previous: ProbeExtractor | null, detectedIndex?: number, unit?: string) {
    return probe({
      field: f, base, marks: m,
      previous: previous ?? undefined,
      use_detected: detectedIndex !== undefined ? true : undefined,
      detected_index: detectedIndex,
      duration_unit: unit ?? durationUnit.current ?? undefined,
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
      // Detection is out of candidates: offer the model's first guess, once.
      if (!proposed.current.has(i)) {
        proposed.current.add(i)
        const guess = await probe({ field: FIELDS[i].key, base, marks: [], propose: true })
        if (guess.extractor) {
          setResult(guess)
          setCand({ idx, total: idx + 1 })
          setStage('confirm')
          return
        }
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
      if (res.needs_unit && durationUnit.current === null) {
        setPendingUnit({ marks: next, result: res })
        setSelection(null)
        setStage('unit')
        return
      }
      setMarks(next); setPrev(res.extractor); setResult(res)
      setFlagLine(null); setSelection(null); setNotice('')
      setStage('review')
    })
  }

  // The person said what a bare duration number counts; read the marks again with it.
  function chooseUnit(unit: string) {
    if (!pendingUnit) return
    guarded(async () => {
      durationUnit.current = unit
      const res = await run(field.key, pendingUnit.marks, null, undefined, unit)
      if (!res.extractor) { setNotice(res.message ?? 'No rule matches that selection.'); setStage('pick'); return }
      setMarks(pendingUnit.marks); setPrev(res.extractor); setResult(res)
      setPendingUnit(null); setNotice('')
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
    durationUnit.current = null
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
    return (
      <div className="psc-wizard" data-testid={tp}>
        {header}
        <p className="psc-prose">looking…</p>
        <SamplePreview sample={sample} testid={`${tp}-preview`} />
        {messages}
      </div>
    )
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
    return (
      <div className="psc-wizard" data-testid={tp}>
        {header}
        <p className="psc-prose">
          Found a {field.label} on <strong>{result.matched} of {result.total}</strong> lines
          {cand.total > 1 ? <> (candidate {cand.idx + 1} of {cand.total})</> : null}. Is this it?
        </p>
        <SamplePreview sample={sample} result={result} testid={`${tp}-candidate`} />
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

  if (stage === 'unit' && pendingUnit) {
    return (
      <div className="psc-wizard" data-testid={tp}>
        {header}
        <p className="psc-prose">
          What are these numbers counted in? You selected{' '}
          {pendingUnit.marks.filter(m => !m.none).map(m => `“${m.text}”`).join(', ')}.
          Durations are stored as nanoseconds.
        </p>
        <div className="psc-actions">
          {[['ns', 'nanoseconds'], ['us', 'microseconds'], ['ms', 'milliseconds'], ['s', 'seconds']].map(([u, label]) => (
            <button key={u} className="tui-btn" disabled={busy} onClick={() => chooseUnit(u)}
              data-testid={`${tp}-unit-${u}-button`}>{label}</button>
          ))}
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
        <SamplePreview sample={sample} testid={`${tp}-preview`} />
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
          selection={selection}
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
              <span className={`psc-wiz-mark${mk ? (mk.none ? ' psc-mark-none' : ' psc-mark-user') : r?.matched ? '' : ' psc-mark-miss'}`}
                data-testid={`${tp}-mark-${i}`}>{mk ? (mk.none ? '✗' : '✓') : r?.matched ? '✓' : '·'}</span>
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
