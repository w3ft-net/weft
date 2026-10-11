import { useEffect, useRef, type MutableRefObject } from 'react'
import { reportModalClosed } from '../wasm/network'

// useReportModalClosed tells the server a shell modal went away, against the
// typed line that opened it, so the tour can complete a lesson on dismissal.
// It reports from unmount so every way of closing counts (Esc, the close
// button, the backdrop, another modal replacing it).
//
// `shown` says whether the modal actually displayed something; one dismissed
// while loading, or after an error, did not teach the lesson.
//
// The report is deferred a tick and cancelled if the component remounts: dev
// builds (StrictMode) mount, unmount and remount every component, and that
// simulated unmount is not the user closing anything.
export function useReportModalClosed(pipeline: string | undefined, shown: MutableRefObject<boolean>) {
  const pending = useRef<ReturnType<typeof setTimeout> | null>(null)
  useEffect(() => {
    if (pending.current) {
      clearTimeout(pending.current)
      pending.current = null
    }
    return () => {
      if (pipeline && shown.current) {
        pending.current = setTimeout(() => void reportModalClosed(pipeline), 0)
      }
    }
  }, [pipeline, shown])
}
