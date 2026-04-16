import { useEffect, useRef, useState } from 'react'
import './VillainCarousel.css'

const VILLAINS = ['sumo', 'dog', 'horse']
const CYCLE_MS = 6000

interface Props {
  side: 'left' | 'right'
  startIndex?: number
}

export default function VillainCarousel({ side, startIndex = 0 }: Props) {
  const [index, setIndex] = useState(startIndex % VILLAINS.length)
  const [fading, setFading] = useState(false)
  const objRef = useRef<HTMLObjectElement>(null)
  const layerTimer = useRef<ReturnType<typeof setInterval> | null>(null)

  // Cycle through villains — both sides advance simultaneously
  // so they stay offset by their startIndex and never collide.
  useEffect(() => {
    const interval = setInterval(() => {
      setFading(true)
      setTimeout(() => {
        setIndex(prev => (prev + 1) % VILLAINS.length)
        setFading(false)
      }, 600)
    }, CYCLE_MS)

    return () => clearInterval(interval)
  }, [])

  // Layer 1/2 toggle animation within each SVG
  useEffect(() => {
    const obj = objRef.current
    if (!obj) return

    function startLayerAnim() {
      const doc = obj!.contentDocument
      if (!doc) return
      const l1 = doc.getElementById('Layer_1')
      const l2 = doc.getElementById('Layer_2')
      if (!l1 || !l2) return
      l1.style.transition = 'opacity 0.25s'
      l2.style.transition = 'opacity 0.25s'
      let show1 = true
      l1.style.opacity = '1'
      l2.style.opacity = '0'

      if (layerTimer.current) clearInterval(layerTimer.current)
      layerTimer.current = setInterval(() => {
        show1 = !show1
        l1.style.opacity = show1 ? '1' : '0'
        l2.style.opacity = show1 ? '0' : '1'
      }, 800)
    }

    obj.addEventListener('load', startLayerAnim)
    // If already loaded
    if (obj.contentDocument?.getElementById('Layer_1')) {
      startLayerAnim()
    }

    return () => {
      obj.removeEventListener('load', startLayerAnim)
      if (layerTimer.current) clearInterval(layerTimer.current)
    }
  }, [index]) // re-run when villain changes

  const villain = VILLAINS[index]
  const sizeStyle = villain === 'horse' ? { width: 160 } : { width: 220 }

  return (
    <div className={`villain-carousel villain-carousel-${side}`} data-testid={`villain-carousel-${side}`}>
      <div className={`villain-fade ${fading ? 'fading' : ''}`}>
        <object
          ref={objRef}
          key={villain} // force remount on villain change
          data={`/assets/${villain}.svg`}
          type="image/svg+xml"
          style={sizeStyle}
          aria-label={villain}
        />
      </div>
    </div>
  )
}
