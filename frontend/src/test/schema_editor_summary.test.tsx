// @vitest-environment jsdom
import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'

import SchemaEditor from '../components/SchemaEditor'
import type { Preview, ProbeFn, SourceDetection } from '../lib/api'

afterEach(cleanup)

const probe: ProbeFn = async () => ({ extractor: null, results: [], matched: 0, total: 1 })

describe('SchemaEditor when the guided walk is cancelled', () => {
  it('calls onCancel and offers no manual form', async () => {
    let cancelled = 0
    render(
      <SchemaEditor testidPrefix="se" sample={['a line']} detection={{} as SourceDetection}
        probe={probe} onSubmit={async () => {}} onCancel={() => { cancelled++ }} />,
    )
    fireEvent.click(await screen.findByTestId('se-wizard-cancel-button'))
    expect(cancelled).toBe(1)
    expect(screen.queryByTestId('se-advanced-button')).toBeNull()
  })
})

describe('SchemaEditor when the guided walk finishes', () => {
  async function walkToTheEnd() {
    // nothing detected anywhere: say "not expected" for timestamp, level and trace ID, then no spans
    for (const stage of ['expect-no-button', 'expect-no-button', 'expect-no-button']) {
      fireEvent.click(await screen.findByTestId(`se-wizard-${stage}`))
    }
    fireEvent.click(await screen.findByTestId('se-wizard-spans-no-button'))
  }

  it('configures straight away: the walk was the confirmation', async () => {
    const submitted: unknown[] = []
    render(
      <SchemaEditor testidPrefix="se" sample={['a line']} detection={{} as SourceDetection}
        probe={probe} onSubmit={async c => { submitted.push(c) }} />,
    )
    await walkToTheEnd()
    await waitFor(() => expect(submitted).toHaveLength(1))
    // and it is not the detection's guess shown back as if it still needed approving
    expect(screen.queryByTestId('se-format-prose')).toBeNull()
    expect(screen.queryByTestId('se-sample')).toBeNull()
  })

  const preview: Preview = {
    lines: [[{ text: '2026-01-01', field: 'timestamp' }, { text: ' hello' }]],
    fields: ['timestamp'],
  }

  it('shows the preview from onPreview once the walk completes', async () => {
    render(
      <SchemaEditor testidPrefix="se" sample={['a line']} detection={{} as SourceDetection}
        probe={probe} onSubmit={async () => {}} onPreview={async () => preview} />,
    )
    await walkToTheEnd()
    await waitFor(() => expect(screen.getByTestId('se-preview-line-0').textContent).toBe('2026-01-01 hello'))
    expect(screen.queryByTestId('se-preview-key-timestamp')).not.toBeNull()
    expect(screen.queryByTestId('se-value-timestamp')).toBeNull()
  })

  it('still configures when the preview call rejects', async () => {
    const submitted: unknown[] = []
    render(
      <SchemaEditor testidPrefix="se" sample={['a line']} detection={{} as SourceDetection}
        probe={probe} onSubmit={async c => { submitted.push(c) }}
        onPreview={async () => { throw new Error('nope') }} />,
    )
    await walkToTheEnd()
    await waitFor(() => expect(submitted).toHaveLength(1))
    expect(screen.queryByTestId('se-preview')).toBeNull()
    expect(screen.queryByTestId('se-error')).toBeNull()
  })

  it('keeps the values and a retry when configuring fails', async () => {
    let calls = 0
    render(
      <SchemaEditor testidPrefix="se" sample={['a line']} detection={{} as SourceDetection}
        probe={probe} onSubmit={async () => { calls++; if (calls === 1) throw new Error('boom') }} />,
    )
    await walkToTheEnd()
    await waitFor(() => expect(screen.getByTestId('se-error').textContent).toContain('boom'))
    expect(screen.queryByTestId('se-values')).not.toBeNull()
    fireEvent.click(screen.getByTestId('se-submit-button'))
    await waitFor(() => expect(calls).toBe(2))
  })
})
