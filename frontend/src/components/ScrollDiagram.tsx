import { useEffect, useRef, useState, type ReactNode } from 'react'
import './ScrollDiagram.css'

// ── Narrative sidebar copy ────────────────────────────────────────────
// Each block fades in while `progress` is inside [start, end).
const NARRATIVES: { start: number; end: number; title: string; body: ReactNode }[] = [
  {
    start: 0,
    end: 0.45,
    title: 'ingest from anywhere',
    body: <>
      install <em>weftd</em> on a linux host to tail log files and ship{' '}
      <code>/proc</code> metrics over gRPC. for lambda, ECS, API gateway,
      anything in <em>CloudWatch</em> — weft polls a cross-account IAM role.
      no agent required.
    </>,
  },
  {
    start: 0.45,
    end: 0.58,
    title: 'your shell, just bigger',
    body: <>
      log sources are pseudofiles under <code>/log/&lt;ns&gt;/&lt;service&gt;/&lt;kind&gt;.log</code>{' '}
      (an <code>&lt;env&gt;</code> segment slots in after the namespace for env-bearing tokens).
      pipe through <code>awk</code>, <code>jq</code>, <code>grep</code>,{' '}
      <code>sed</code> — meta-vars like <code>$LEVEL</code> and <code>$HOST</code>{' '}
      push down to column scans, not <em>json_extract</em>.
    </>,
  },
  {
    start: 0.58,
    end: 0.71,
    title: 'metrics ride the same wires',
    body: <>
      <em>weftd</em> samples <code>/proc</code> and ships cpu, memory, disk, load,
      network rows as JSON to <code>metrics.log</code>. no second product, no
      second query language — just <code>tail | jq</code>.
    </>,
  },
  {
    start: 0.71,
    end: 0.84,
    title: 'schedule it, alert on it',
    body: <>
      drop a <code># schedule:</code> file in <code>/cron.d/</code>. numeric stdout
      draws a sparkline; pipe to <code>alert above N --notify=&lt;alias&gt;</code> to
      page slack, pagerduty, email, or any webhook. aliases live in{' '}
      <code>/etc/notify.yaml</code>.
    </>,
  },
  {
    start: 0.84,
    end: 1.01,
    title: 'terminal or browser, same shell',
    body: <>
      <code>chart</code> and <code>dashboard</code> render in both. scripts in{' '}
      <code>/bin</code> (bash or lua) compose into pipelines; every save is
      versioned with <code>wit</code>.
    </>,
  },
]

// ── Layout constants ──────────────────────────────────────────────────
const BOX_W = 150
const BOX_H = 70
const BOX_GAP = 20
const ROW_GAP = 30
const FONT = "'JetBrains Mono', 'Fira Code', monospace"
const FONT_SIZE = 11
const LINE_H = 14

const BLUE = '#5fafd7'
const ORANGE = '#ff8700'
const DIM = '#555577'
const BG = '#0f0f1a'

const SVG_W = 560
const SVG_H_DIAGRAM = 500
const SVG_H_SHELL = 500

// ── Service data ──────────────────────────────────────────────────────
// `weftLabel` is what replaces `log` once weftification kicks in.
// EC2 hosts run the weftd agent over gRPC; Lambda/ECS land in
// CloudWatch and are pulled by weft's cross-account poller — two
// real ingest paths.
const ROWS = [
  { labels: ['web-1', 'web-2', 'web-3'], infra: 'EC2', log: '/var/log/*', weftLabel: 'weftd ●' },
  { labels: ['payment', 'notify', 'resize-img'], infra: 'Lambda', log: 'CloudWatch', weftLabel: 'cw poll ◆' },
  { labels: ['importer', 'scheduler'], infra: 'ECS Fargate', log: 'CloudWatch', weftLabel: 'cw poll ◆' },
]

// ── Shell commands ────────────────────────────────────────────────────
const SHELL_COMMANDS = [
  {
    cmd: `cat /log/loom/prod/api/main.log | awk '$LEVEL == "ERROR"' | head -3`,
    output: [
      '14:23:11  api-7f9b   timeout to upstream',
      '14:23:14  api-3a2e   invalid auth token',
      '14:23:17  api-7f9b   redis CLUSTERDOWN',
    ],
  },
  {
    cmd: `tail /log/loom/prod/api/metrics.log | jq 'select(.type=="cpu" and .user>80)'`,
    output: [
      '{"type":"cpu","_host":"web-2","user":94,"idle":3}',
      '{"type":"cpu","_host":"importer","user":88,"idle":11}',
      '{"type":"cpu","_host":"web-2","user":91,"idle":6}',
    ],
  },
  {
    cmd: 'cat /cron.d/error-rate',
    output: [
      '# schedule: every 1m',
      'tail -n 500 /log/loom/prod/api/main.log \\',
      '  | grep ERROR | wc -l \\',
      '  | alert above 50 --notify=ops',
    ],
  },
  {
    // Output is the exact glyph sequence chart.Sparkline() emits for
    // the realistic error-rate series in chart/landing_sparkline_test.go.
    cmd: 'chart error-rate -t sparkline',
    output: [
      '\u2581\u2581\u2581\u2582\u2582\u2582\u2582\u2583\u2583\u2584\u2585\u2584\u2586\u2585\u2587\u2588\u2586\u2584\u2583\u2582\u2582\u2581\u2581\u2581',
    ],
  },
]

// ── Helpers ───────────────────────────────────────────────────────────
function boxX(boxIndex: number, boxCount: number): number {
  const rowW = boxCount * BOX_W + (boxCount - 1) * BOX_GAP
  const startX = (SVG_W - rowW) / 2
  return startX + boxIndex * (BOX_W + BOX_GAP)
}

function rowY(rowIndex: number): number {
  return 20 + rowIndex * (BOX_H + ROW_GAP)
}

// Terminal box position in diagram coordinates
const TERM_BOX = {
  x: SVG_W / 2 - 130,
  y: rowY(2) + BOX_H + ROW_GAP + 40 + 60, // connectorBottom + 40 (ingest) + 60
  w: 130,
  h: 45,
}

// ── Box component ─────────────────────────────────────────────────────
function ServiceBox({ x, y, label, infra, log, weftLabel, weftified }: {
  x: number; y: number; label: string; infra: string; log: string;
  weftLabel: string; weftified: boolean
}) {
  return (
    <g>
      <rect x={x} y={y} width={BOX_W} height={BOX_H}
        fill="none" stroke={BLUE} strokeWidth={1.5} rx={2} />
      <text x={x + 10} y={y + 18} fill={BLUE} fontSize={FONT_SIZE} fontFamily={FONT}>{label}</text>
      <text x={x + 10} y={y + 32} fill={DIM} fontSize={FONT_SIZE - 1} fontFamily={FONT}>{infra}</text>
      <text x={x + 10} y={y + 48} fontSize={FONT_SIZE - 1} fontFamily={FONT}
        fill={weftified ? ORANGE : DIM}
        style={{ transition: 'fill 0.4s' }}
      >{weftified ? weftLabel : log}</text>
    </g>
  )
}

// ── Main component ────────────────────────────────────────────────────
/*
  Phases:
    0.00 - 0.05  empty
    0.05 - 0.12  row 1 (EC2)
    0.12 - 0.20  row 2 (Lambda)
    0.20 - 0.28  row 3 (ECS) + "$ grep ... where?"
    0.28 - 0.36  weftification
    0.36 - 0.45  zoom: terminal box scales up, everything else fades
    0.45 - 1.00  shell simulation
*/
export default function ScrollDiagram() {
  const containerRef = useRef<HTMLDivElement>(null)
  const [progress, setProgress] = useState(0)

  useEffect(() => {
    function onScroll() {
      const container = containerRef.current
      if (!container) return
      const rect = container.getBoundingClientRect()
      const viewH = window.innerHeight
      const totalScroll = container.offsetHeight - viewH
      if (totalScroll <= 0) return
      setProgress(Math.min(1, Math.max(0, -rect.top / totalScroll)))
    }
    window.addEventListener('scroll', onScroll, { passive: true })
    onScroll()
    return () => window.removeEventListener('scroll', onScroll)
  }, [])

  const visibleRows = progress < 0.05 ? 0 : progress < 0.12 ? 1 : progress < 0.20 ? 2 : 3
  const weftified = progress >= 0.28
  const zooming = progress >= 0.36 && progress < 0.45
  const showShell = progress >= 0.45
  const zoomPct = zooming ? (progress - 0.36) / 0.09 : showShell ? 1 : 0
  const shellProgress = showShell ? Math.min(1, (progress - 0.45) / 0.52) : 0

  const connectorBottomY = rowY(2) + BOX_H + ROW_GAP
  const ingestBoxY = connectorBottomY + 40
  const termBoxY = ingestBoxY + 60
  const browserBoxY = ingestBoxY + 60
  const cx = SVG_W / 2

  // ── Diagram elements ──
  function diagramContent(): ReactNode {
    // Everything except the terminal box fades out during zoom
    const otherOpacity = 1 - zoomPct

    return (
      <>
        {/* Service boxes */}
        <g style={{ opacity: otherOpacity, transition: 'opacity 0.3s' }}>
          {ROWS.map((row, ri) => {
            if (ri >= visibleRows) return null
            return row.labels.map((label, bi) => (
              <ServiceBox
                key={`${ri}-${bi}`}
                x={boxX(bi, row.labels.length)}
                y={rowY(ri)}
                label={label}
                infra={row.infra}
                log={row.log}
                weftLabel={row.weftLabel}
                weftified={weftified}
              />
            ))
          })}
        </g>

        {/* Connection lines */}
        {weftified && visibleRows >= 3 && (
          <g style={{ opacity: otherOpacity, transition: 'opacity 0.3s' }}>
            {connectionLines()}
          </g>
        )}

        {/* Grep question */}
        {!weftified && visibleRows >= 3 && (
          <text x={cx} y={connectorBottomY + 30} fill={DIM}
            fontSize={FONT_SIZE} fontFamily={FONT} textAnchor="middle"
          >$ grep ... where?</text>
        )}

        {/* Ingest box + browser (fade with others) */}
        {weftified && visibleRows >= 3 && (
          <g style={{ opacity: otherOpacity, transition: 'opacity 0.3s' }}>
            <rect x={cx - 80} y={ingestBoxY} width={160} height={40}
              fill="none" stroke={ORANGE} strokeWidth={2} rx={2} />
            <text x={cx} y={ingestBoxY + 24} fill={ORANGE} fontSize={FONT_SIZE}
              fontFamily={FONT} textAnchor="middle">weft ingest</text>

            <line x1={cx - 30} y1={ingestBoxY + 40} x2={cx - 60} y2={termBoxY}
              stroke={ORANGE} strokeWidth={1.5} />
            <line x1={cx + 30} y1={ingestBoxY + 40} x2={cx + 60} y2={browserBoxY}
              stroke={ORANGE} strokeWidth={1.5} />

            {/* Browser box */}
            <rect x={cx + 5} y={browserBoxY} width={130} height={45}
              fill="none" stroke={ORANGE} strokeWidth={1.5} rx={2} />
            <text x={cx + 15} y={browserBoxY + 18} fill={ORANGE} fontSize={FONT_SIZE}
              fontFamily={FONT}>browser</text>
            <text x={cx + 15} y={browserBoxY + 34} fill="#00afff" fontSize={FONT_SIZE - 1}
              fontFamily={FONT}>w3ft.net</text>
          </g>
        )}

        {/* Terminal box — this one zooms up */}
        {weftified && visibleRows >= 3 && terminalBox()}
      </>
    )
  }

  function connectionLines(): ReactNode {
    const fadeIn = Math.min(1, (progress - 0.28) / 0.06)
    const paths: ReactNode[] = []
    const trunkX = cx

    for (let ri = 0; ri < ROWS.length; ri++) {
      const row = ROWS[ri]
      const y1 = rowY(ri) + BOX_H
      const y2 = connectorBottomY + 20

      for (let bi = 0; bi < row.labels.length; bi++) {
        const bx = boxX(bi, row.labels.length) + BOX_W / 2
        const dashLen = 200
        const dashOffset = (1 - fadeIn) * dashLen

        paths.push(
          <line key={`v-${ri}-${bi}`} x1={bx} y1={y1} x2={bx} y2={y2}
            stroke={ORANGE} strokeWidth={1.5}
            strokeDasharray={dashLen} strokeDashoffset={dashOffset}
            style={{ transition: 'stroke-dashoffset 0.5s ease' }} />
        )
        if (bx !== trunkX) {
          paths.push(
            <line key={`h-${ri}-${bi}`} x1={bx} y1={y2} x2={trunkX} y2={y2}
              stroke={ORANGE} strokeWidth={1.5}
              strokeDasharray={dashLen} strokeDashoffset={dashOffset}
              style={{ transition: 'stroke-dashoffset 0.5s ease' }} />
          )
        }
      }
    }

    paths.push(
      <line key="trunk" x1={trunkX} y1={connectorBottomY + 20} x2={trunkX} y2={ingestBoxY}
        stroke={ORANGE} strokeWidth={2}
        style={{ opacity: fadeIn, transition: 'opacity 0.3s' }} />
    )

    return <>{paths}</>
  }

  // Terminal box that zooms to fill viewport
  function terminalBox(): ReactNode {
    // Interpolate from small box to full SVG viewport
    const pad = 4
    const fromX = TERM_BOX.x
    const fromY = termBoxY
    const fromW = TERM_BOX.w
    const fromH = TERM_BOX.h
    const toX = pad
    const toY = pad
    const toW = SVG_W - pad * 2
    const toH = SVG_H_SHELL - pad * 2

    const lerp = (a: number, b: number) => a + (b - a) * zoomPct
    const bx = lerp(fromX, toX)
    const by = lerp(fromY, toY)
    const bw = lerp(fromW, toW)
    const bh = lerp(fromH, toH)

    // Text scales with the box
    const textScale = bw / fromW
    const labelOpacity = 1 - zoomPct  // "terminal" label fades out
    const promptOpacity = zoomPct < 0.5 ? 1 : 1  // weft >_ stays

    return (
      <g>
        <rect x={bx} y={by} width={bw} height={bh}
          fill={BG} stroke={zoomPct > 0.5 ? '#252540' : ORANGE}
          strokeWidth={1.5} rx={2}
          style={{ transition: 'stroke 0.3s' }} />
        {/* "terminal" label fades during zoom */}
        <text x={bx + 10} y={by + 18 * Math.min(textScale, 1.5)} fill={ORANGE}
          fontSize={FONT_SIZE} fontFamily={FONT}
          style={{ opacity: labelOpacity, transition: 'opacity 0.3s' }}
        >terminal</text>
        {/* "weft >_" stays and repositions */}
        <text x={bx + 10} y={by + (zoomPct > 0.8 ? 24 : 34 * Math.min(textScale, 1.2))}
          fill="#00afff" fontSize={FONT_SIZE} fontFamily={FONT}
          style={{ opacity: promptOpacity }}
        >weft &gt;{zoomPct < 0.8 ? '_' : ' \u2588'}</text>
      </g>
    )
  }

  // ── Shell content (plays inside the zoomed terminal) ──
  function shellContent(): ReactNode {
    const lines: ReactNode[] = []
    let y = 40 // below the "weft >" prompt at the top
    const totalCommands = SHELL_COMMANDS.length

    for (let ci = 0; ci < totalCommands; ci++) {
      const cmdStart = ci / totalCommands
      const cmdEnd = (ci + 1) / totalCommands
      const cmdProg = Math.min(1, Math.max(0, (shellProgress - cmdStart) / (cmdEnd - cmdStart)))
      if (cmdProg <= 0) break

      const entry = SHELL_COMMANDS[ci]
      const typePct = Math.min(1, cmdProg / 0.25)
      const charsToShow = Math.floor(typePct * entry.cmd.length)
      const typing = typePct < 1

      if (ci > 0) y += LINE_H * 0.8

      lines.push(
        <text key={`p-${ci}`} x={12} y={y} fill="#00afff" fontSize={FONT_SIZE} fontFamily={FONT}>
          weft &gt;{' '}
          <tspan fill="#e0e0f0">{entry.cmd.slice(0, charsToShow)}</tspan>
          {typing && <tspan fill="#00afff">{'\u2588'}</tspan>}
        </text>
      )

      if (!typing) {
        const outPct = Math.min(1, (cmdProg - 0.25) / 0.75)
        const linesToShow = Math.ceil(outPct * entry.output.length)
        y += LINE_H * 0.4

        for (let li = 0; li < linesToShow; li++) {
          y += LINE_H
          lines.push(
            <text key={`o-${ci}-${li}`} x={12} y={y} fill="#5fff87"
              fontSize={FONT_SIZE} fontFamily={FONT}>{entry.output[li]}</text>
          )
        }
      }
      y += LINE_H
    }

    if (shellProgress >= 0.95) {
      y += LINE_H
      lines.push(
        <text key="final" x={12} y={y} fill="#00afff" fontSize={FONT_SIZE} fontFamily={FONT}>
          weft &gt; <tspan className="cursor-blink">{'\u2588'}</tspan>
        </text>
      )
    }

    return <g>{lines}</g>
  }

  // ── Choose viewBox height based on phase ──
  const svgH = showShell ? SVG_H_SHELL : SVG_H_DIAGRAM

  return (
    <div className="scroll-diagram-container" ref={containerRef}>
      <div className="scroll-diagram-sticky">
        <div className="scroll-diagram-layout">
          <aside className="scroll-narrative" data-testid="scroll-narrative">
            {NARRATIVES.map((n, i) => {
              const active = progress >= n.start && progress < n.end
              return (
                <div
                  key={i}
                  className="narrative-block"
                  data-testid={`narrative-block-${i}`}
                  style={{ opacity: active ? 1 : 0 }}
                  aria-hidden={!active}
                >
                  <h3>{n.title}</h3>
                  <p>{n.body}</p>
                </div>
              )
            })}
          </aside>

          <svg
            className="diagram-svg"
            viewBox={`0 0 ${SVG_W} ${svgH}`}
            preserveAspectRatio="xMidYMin meet"
            data-testid="scroll-diagram"
            style={{ transition: 'all 0.5s ease' }}
          >
            <rect width={SVG_W} height={svgH} fill={BG} rx={4} />

            {!showShell && diagramContent()}

            {showShell && (
              <>
                {/* Full-viewport terminal frame */}
                <rect x={4} y={4} width={SVG_W - 8} height={SVG_H_SHELL - 8}
                  fill={BG} stroke="#252540" strokeWidth={1} rx={2} />
                {shellContent()}
              </>
            )}

            {/* Border */}
            <rect width={SVG_W} height={svgH} fill="none"
              stroke="#252540" strokeWidth={1} rx={4} />
          </svg>
        </div>
      </div>
    </div>
  )
}
