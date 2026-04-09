import { useState, useEffect, useRef } from 'react'
import './TerminalDemo.css'

interface Line {
  type: 'prompt' | 'output' | 'empty'
  text: string
  delay?: number
}

const DEMO_LINES: Line[] = [
  { type: 'prompt', text: 'cat vimgym.log | grep ERROR | head -5', delay: 60 },
  { type: 'output', text: '2026-04-06 10:00:01 ERROR vimgym i-001 connection timeout' },
  { type: 'output', text: '2026-04-06 10:02:00 ERROR vimgym i-001 null pointer exception' },
  { type: 'output', text: '2026-04-06 10:20:00 ERROR vimgym i-003 timeout waiting for response' },
  { type: 'output', text: '2026-04-06 11:01:00 ERROR vimgym i-001 500 internal server error' },
  { type: 'output', text: '2026-04-06 11:30:00 ERROR vimgym i-001 connection timeout to upstream' },
  { type: 'empty', text: '' },
  { type: 'prompt', text: 'cat warp.log | awk \'$LEVEL == "ERROR"\' | wc -l', delay: 45 },
  { type: 'output', text: '47' },
  { type: 'empty', text: '' },
  { type: 'prompt', text: 'tail -f grantcentral.log | grep -i auth', delay: 55 },
  { type: 'output', text: '2026-04-06 12:40:00 ERROR authentication failed for tenant acme' },
  { type: 'output', text: '2026-04-06 12:41:15 INFO  auth token refreshed for user u-3847' },
  { type: 'output', text: '2026-04-06 12:41:22 WARN  auth rate limit approaching for 10.0.3.12' },
]

export default function TerminalDemo() {
  const [visibleLines, setVisibleLines] = useState<{ type: string; text: string; typing?: boolean }[]>([])
  const [currentLine, setCurrentLine] = useState(0)
  const [charIndex, setCharIndex] = useState(0)
  const [isTyping, setIsTyping] = useState(false)
  const termRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (currentLine >= DEMO_LINES.length) {
      // Restart after pause
      const t = setTimeout(() => {
        setVisibleLines([])
        setCurrentLine(0)
        setCharIndex(0)
      }, 4000)
      return () => clearTimeout(t)
    }

    const line = DEMO_LINES[currentLine]

    if (line.type === 'prompt') {
      setIsTyping(true)
      if (charIndex === 0) {
        setVisibleLines(prev => [...prev, { type: 'prompt', text: '', typing: true }])
      }

      if (charIndex < line.text.length) {
        const t = setTimeout(() => {
          setVisibleLines(prev => {
            const updated = [...prev]
            updated[updated.length - 1] = {
              type: 'prompt',
              text: line.text.slice(0, charIndex + 1),
              typing: true,
            }
            return updated
          })
          setCharIndex(charIndex + 1)
        }, line.delay || 50)
        return () => clearTimeout(t)
      } else {
        // Done typing prompt
        setVisibleLines(prev => {
          const updated = [...prev]
          updated[updated.length - 1] = { type: 'prompt', text: line.text }
          return updated
        })
        setIsTyping(false)
        const t = setTimeout(() => {
          setCurrentLine(currentLine + 1)
          setCharIndex(0)
        }, 500)
        return () => clearTimeout(t)
      }
    } else {
      // Output or empty — appear instantly after a short delay
      const t = setTimeout(() => {
        setVisibleLines(prev => [...prev, { type: line.type, text: line.text }])
        setCurrentLine(currentLine + 1)
        setCharIndex(0)
      }, line.type === 'empty' ? 200 : 80)
      return () => clearTimeout(t)
    }
  }, [currentLine, charIndex])

  useEffect(() => {
    if (termRef.current) {
      termRef.current.scrollTop = termRef.current.scrollHeight
    }
  }, [visibleLines])

  return (
    <div className="terminal" data-testid="terminal-demo">
      <div className="terminal-chrome">
        <div className="terminal-dots">
          <span className="dot red" />
          <span className="dot yellow" />
          <span className="dot green" />
        </div>
        <span className="terminal-title">weft — w3ft.net</span>
      </div>
      <div className="terminal-body" ref={termRef}>
        {visibleLines.map((line, i) => (
          <div key={i} className={`terminal-line ${line.type}`}>
            {line.type === 'prompt' && (
              <span className="terminal-prompt">weft &gt; </span>
            )}
            <span className={line.type === 'prompt' ? 'terminal-command' : 'terminal-output'}>
              {line.text}
            </span>
            {line.typing && <span className="terminal-cursor">▊</span>}
          </div>
        ))}
        {!isTyping && currentLine < DEMO_LINES.length && (
          <div className="terminal-line prompt">
            <span className="terminal-prompt">weft &gt; </span>
            <span className="terminal-cursor">▊</span>
          </div>
        )}
      </div>
    </div>
  )
}
