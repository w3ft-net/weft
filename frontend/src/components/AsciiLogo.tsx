import './AsciiLogo.css'

// Exact port of shell/logo.go — same bitmaps, same ANSI 256-color indices.

const letterW = [
  '##    ##    ##',
  '##    ##    ##',
  ' ##  ####  ## ',
  '  ####  ####  ',
  '   ##    ##   ',
]
const letterE = [
  '##########',
  '        ##',
  '  ########',
  '        ##',
  '##########',
]
const letterF = [
  '##########',
  '##        ',
  '########  ',
  '##        ',
  '##        ',
]
const letterT = [
  '##########',
  '    ##    ',
  '    ##    ',
  '    ##    ',
  '    ##    ',
]

const GAP = '   '

// ANSI 256-color index → hex. Only the indices used by the logo.
const ansi256: Record<number, string> = {
  27:  '#005fff', 32:  '#0087d7', 33:  '#0087ff', 39:  '#00afff',
  63:  '#5f5fff', 68:  '#5f87d7', 69:  '#5f87ff', 74:  '#5fafd7',
  75:  '#5fafff', 105: '#8787ff',
  173: '#d7af5f', 208: '#ff8700', 209: '#ff875f', 214: '#ffaf00',
  215: '#ffaf5f',
}

// Warp thread colors (cool blues/indigos) — one per column
const warpColors = [33, 69, 39, 63, 75, 27, 68, 105, 32, 74]
// Weft thread colors (warm oranges/golds) — one per row
const weftColors = [208, 173, 214, 209, 215]

function buildLogo(): { char: string; color: string }[][] {
  const lines: string[] = []
  for (let r = 0; r < letterW.length; r++) {
    lines.push(letterW[r] + GAP + letterE[r] + GAP + letterF[r] + GAP + letterT[r])
  }

  return lines.map((line, r) =>
    Array.from(line).map((ch, c) => {
      if (ch === ' ') return { char: ' ', color: '' }
      // Interlace: even (r+c) = warp on top (║), odd = weft on top (═)
      if ((r + c) % 2 === 0) {
        return { char: '║', color: ansi256[warpColors[c % warpColors.length]] }
      } else {
        return { char: '═', color: ansi256[weftColors[r % weftColors.length]] }
      }
    })
  )
}

const logoRows = buildLogo()

export default function AsciiLogo() {
  return (
    <pre className="ascii-logo" data-testid="ascii-logo" aria-label="weft">{
      logoRows.map((row, r) =>
        <span key={r}>{
          row.map((cell, c) =>
            cell.color
              ? <span key={c} style={{ color: cell.color }}>{cell.char}</span>
              : <span key={c}>{cell.char}</span>
          )
        }{'\n'}</span>
      )
    }</pre>
  )
}
