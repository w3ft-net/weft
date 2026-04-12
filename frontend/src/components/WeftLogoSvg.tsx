import './WeftLogoSvg.css'

const W = [
  '##    ##    ##',
  '##    ##    ##',
  ' ##  ####  ## ',
  '  ####  ####  ',
  '   ##    ##   ',
]
const E = [
  '##########',
  '        ##',
  '  ########',
  '        ##',
  '##########',
]
const F = [
  '##########',
  '##        ',
  '########  ',
  '##        ',
  '##        ',
]
const T = [
  '##########',
  '    ##    ',
  '    ##    ',
  '    ##    ',
  '    ##    ',
]

const GAP = '   '

const ansi256: Record<number, string> = {
  27: '#005fff', 32: '#0087d7', 33: '#0087ff', 39: '#00afff',
  63: '#5f5fff', 68: '#5f87d7', 69: '#5f87ff', 74: '#5fafd7',
  75: '#5fafff', 105: '#8787ff',
  173: '#d7af5f', 208: '#ff8700', 209: '#ff875f', 214: '#ffaf00',
  215: '#ffaf5f',
}

const warpColors = [33, 69, 39, 63, 75, 27, 68, 105, 32, 74]
const weftColors = [208, 173, 214, 209, 215]

interface Cell { r: number; c: number; color: string; warp: boolean }

function buildCells(): Cell[] {
  const lines: string[] = []
  for (let r = 0; r < W.length; r++) {
    lines.push(W[r] + GAP + E[r] + GAP + F[r] + GAP + T[r])
  }
  const cells: Cell[] = []
  for (let r = 0; r < lines.length; r++) {
    for (let c = 0; c < lines[r].length; c++) {
      if (lines[r][c] === ' ') continue
      const isWarp = (r + c) % 2 === 0
      cells.push({
        r, c,
        color: isWarp
          ? ansi256[warpColors[c % warpColors.length]]
          : ansi256[weftColors[r % weftColors.length]],
        warp: isWarp,
      })
    }
  }
  return cells
}

const cells = buildCells()
const COLS = (W[0] + GAP + E[0] + GAP + F[0] + GAP + T[0]).length
const ROWS = W.length

// Cell dimensions — match monospace terminal aspect ratio (~0.6 width:height)
const CW = 7
const CH = 14
const SVG_W = COLS * CW
const SVG_H = ROWS * CH

interface Props {
  className?: string
}

export default function WeftLogoSvg({ className }: Props) {
  return (
    <svg
      className={`weft-logo-svg ${className || ''}`}
      viewBox={`0 0 ${SVG_W} ${SVG_H}`}
      xmlns="http://www.w3.org/2000/svg"
      role="img"
      aria-label="weft"
      data-testid="weft-logo-svg"
    >
      <defs>
        <filter id="logo-glow" x="-15%" y="-25%" width="130%" height="150%">
          <feGaussianBlur in="SourceGraphic" stdDeviation="3" result="blur" />
          <feColorMatrix in="blur" type="matrix"
            values="0 0 0 0 0.5
                    0 0 0 0 0.5
                    0 0 0 0 0.55
                    0 0 0 0 0.35 0"
            result="glow" />
          <feMerge>
            <feMergeNode in="glow" />
            <feMergeNode in="SourceGraphic" />
          </feMerge>
        </filter>
      </defs>
      <g filter="url(#logo-glow)">
        {cells.map((cell, i) => {
          const x = cell.c * CW
          const y = cell.r * CH
          if (cell.warp) {
            // Warp thread (║): vertical bar, thick enough to stay visible at small sizes
            const barW = CW * 0.55
            return (
              <rect
                key={i}
                x={x + (CW - barW) / 2}
                y={y}
                width={barW}
                height={CH}
                fill={cell.color}
              />
            )
          } else {
            // Weft thread (═): horizontal bar
            const barH = CH * 0.45
            return (
              <rect
                key={i}
                x={x}
                y={y + (CH - barH) / 2}
                width={CW}
                height={barH}
                fill={cell.color}
              />
            )
          }
        })}
      </g>
    </svg>
  )
}
