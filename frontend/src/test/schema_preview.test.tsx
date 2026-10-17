// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor, fireEvent } from '@testing-library/react'

import SchemaPreview from '../components/SchemaPreview'
import ConfiguredSourcesList from '../components/ConfiguredSourcesList'
import * as api from '../lib/api'
import type { Preview, SourceInfo } from '../lib/api'

afterEach(() => { cleanup(); vi.restoreAllMocks() })

const preview: Preview = {
  lines: [
    [
      { text: '2026-01-01T00:00:00Z', field: 'timestamp' },
      { text: ' ' },
      { text: 'INFO', field: 'level' },
      { text: ' started' },
    ],
    [{ text: 'plain line' }],
  ],
  fields: ['timestamp', 'level'],
}

describe('SchemaPreview', () => {
  it('reproduces each line exactly', () => {
    render(<SchemaPreview testidPrefix="t" preview={preview} />)
    expect(screen.getByTestId('t-preview-line-0').textContent).toBe('2026-01-01T00:00:00Z INFO started')
    expect(screen.getByTestId('t-preview-line-1').textContent).toBe('plain line')
  })

  it('colours extracted segments by field and leaves plain text uncoloured', () => {
    render(<SchemaPreview testidPrefix="t" preview={preview} />)
    const line = screen.getByTestId('t-preview-line-0')
    const hl = line.querySelectorAll('.sp-hl')
    expect(hl).toHaveLength(2)
    expect(hl[0].className).toContain('sp-f-timestamp')
    expect(hl[1].className).toContain('sp-f-level')
    expect(screen.getByTestId('t-preview-line-1').querySelector('.sp-hl')).toBeNull()
  })

  it('keys only the fields that appeared, with square glyphs and labels', () => {
    render(<SchemaPreview testidPrefix="t" preview={{ ...preview, fields: ['timestamp', 'trace_id'] }} />)
    expect(screen.getByTestId('t-preview-key').textContent).toBe('■timestamp■trace ID')
    expect(screen.queryByTestId('t-preview-key-level')).toBeNull()
    expect(screen.getByTestId('t-preview-key-trace_id').textContent).toContain('■')
  })
})

describe('ConfiguredSourcesList schema panel', () => {
  const sources = [{ service: 'web', source_type: 'weftd' } as SourceInfo]

  async function open(schema: api.SourceSchema) {
    vi.spyOn(api, 'getSourceSchema').mockResolvedValue(schema)
    render(<ConfiguredSourcesList namespace="ns" sources={sources} onChanged={() => {}} />)
    fireEvent.click(screen.getByTestId('configured-source-web-viewschema-button'))
  }

  it('shows the preview instead of the YAML', async () => {
    await open({ status: 'configured', schema_yaml: 'format: text', preview })
    await waitFor(() => expect(screen.queryByTestId('configured-source-web-schema-preview')).not.toBeNull())
    expect(screen.getByTestId('configured-source-web-schema-panel').textContent).not.toContain('format: text')
  })

  it('shows the note when there is no preview', async () => {
    await open({ status: 'configured', schema_yaml: 'format: text', preview_note: 'no recent lines' })
    await waitFor(() => expect(screen.getByTestId('configured-source-web-schema-note').textContent).toBe('no recent lines'))
  })
})

describe('no advanced schema controls', () => {
  it('has no advanced button anywhere in the schema flow sources', async () => {
    const mods = import.meta.glob('../components/*.tsx', { query: '?raw', import: 'default', eager: true }) as Record<string, string>
    for (const [name, src] of Object.entries(mods)) {
      if (name.includes('AddKinesisSource')) continue
      expect(src, name).not.toMatch(/-advanced-button|psc-advanced/)
    }
  })
})
