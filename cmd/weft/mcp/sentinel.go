package mcp

import (
	"bytes"
	"fmt"
)

// sentinelOpen and sentinelClose bracket every block of log content
// returned to the model. The Initialize-time instructions tell the
// model these blocks contain untrusted data; the brackets give it a
// reliable string to recognise.
const (
	sentinelOpen  = "<weft-data>\n"
	sentinelClose = "\n</weft-data>"
)

// capWriter is an io.Writer that records output up to maxBytes
// across at most maxRows newline-terminated rows. Once either limit
// is reached, further writes are silently discarded and Truncated
// becomes true.
//
// Both limits matter: maxRows protects against pipelines that emit
// many small rows (typical log queries), and maxBytes protects
// against a single oversized row.
type capWriter struct {
	buf       bytes.Buffer
	rows      int
	maxRows   int
	maxBytes  int
	truncated bool
}

func newCapWriter(maxRows, maxBytes int) *capWriter {
	return &capWriter{maxRows: maxRows, maxBytes: maxBytes}
}

// Write implements io.Writer. It accepts whole p in the common case
// where the budget hasn't been blown, and on the over-budget write
// it accepts as much as it can and then flips Truncated.
func (c *capWriter) Write(p []byte) (int, error) {
	if c.truncated {
		return len(p), nil
	}
	for i, b := range p {
		if c.buf.Len() >= c.maxBytes {
			c.truncated = true
			return len(p), nil
		}
		if b == '\n' {
			c.rows++
			c.buf.WriteByte('\n')
			if c.rows >= c.maxRows {
				c.truncated = true
				_ = i // remaining bytes are silently discarded
				return len(p), nil
			}
			continue
		}
		c.buf.WriteByte(b)
	}
	return len(p), nil
}

// wrap renders the captured output as a model-facing string: the
// data block sentinel-wrapped, plus a notice block when the writer
// truncated. The notice is rendered as a separate block so a model
// scanning for log content via the data sentinels won't pick up
// the notice text by accident.
func (c *capWriter) wrap() string {
	body := c.buf.Bytes()
	// Trim a trailing newline so the close sentinel sits flush.
	if n := len(body); n > 0 && body[n-1] == '\n' {
		body = body[:n-1]
	}
	out := sentinelOpen + string(body) + sentinelClose
	if c.truncated {
		out += fmt.Sprintf("\n<weft-notice>truncated: output capped at %d rows or %d bytes; add `| head N` or a tighter filter to narrow it</weft-notice>",
			c.maxRows, c.maxBytes)
	}
	return out
}
