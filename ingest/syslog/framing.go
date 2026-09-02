package syslog

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"

	"github.com/leodido/go-syslog/v4/rfc5424"
)

// DefaultMaxFrame is the RFC 5425 ceiling for a syslog-over-TLS message
// and the cap weft applies to any single frame regardless of transport.
const DefaultMaxFrame = 64 << 10

// Framing selects the RFC 6587 transport framing.
type Framing int

const (
	// Detect sniffs the first byte of the stream: a digit means
	// octet-counting ("MSG-LEN SP MSG"), anything else means
	// non-transparent (LF-delimited). Use this for a customer-run relay
	// (rsyslog/Vector) whose framing is not known in advance.
	Detect Framing = iota
	// OctetCounting is RFC 6587 §3.4.1 — what Heroku's drain and
	// (expected) Render's stream use.
	OctetCounting
	// NonTransparent is RFC 6587 §3.4.2 with an LF trailer.
	NonTransparent
)

// ParseStream reads RFC 5424 frames from r and calls fn once per frame.
// A frame that does not parse calls fn with a zero Frame and a non-nil
// error; a caller wanting Heroku's "skip the bad frame, keep the batch"
// behaviour just returns early on a non-nil error. maxFrame caps one
// frame's length; pass 0 for DefaultMaxFrame.
//
// It blocks until r is exhausted, so a long-lived connection (the
// Render listener) drives fn incrementally as frames arrive. In
// octet-counting mode a framing error (unreadable length, truncated
// body) is delivered to fn and ends the stream — the byte offset of the
// next frame is no longer knowable. In LF mode every line stands alone,
// so a bad line is reported and skipped. A plain read error on r ends
// the stream like a clean EOF.
func ParseStream(r io.Reader, framing Framing, maxFrame int, fn func(Frame, error)) {
	if maxFrame <= 0 {
		maxFrame = DefaultMaxFrame
	}
	br := bufio.NewReaderSize(r, 4096)

	if framing == Detect {
		b, err := br.Peek(1)
		if err != nil {
			return // empty stream (or immediate read error) → no frames
		}
		if b[0] >= '0' && b[0] <= '9' {
			framing = OctetCounting
		} else {
			framing = NonTransparent
		}
	}

	// One machine, reused across frames. Best-effort tolerates minor
	// deviations (e.g. a trailing BOM on MSG); a frame that still fails
	// is surfaced as an error and skipped by the caller.
	machine := rfc5424.NewMachine(rfc5424.WithBestEffort())
	deliver := func(raw []byte) {
		msg, err := machine.Parse(raw)
		if err != nil {
			fn(Frame{}, err)
			return
		}
		sm, ok := msg.(*rfc5424.SyslogMessage)
		if !ok {
			fn(Frame{}, fmt.Errorf("syslog: parser returned %T", msg))
			return
		}
		fn(fromRFC5424(sm), nil)
	}

	if framing == NonTransparent {
		scanLF(br, maxFrame, deliver, fn)
		return
	}
	scanOctetCounting(br, maxFrame, deliver, fn)
}

// scanOctetCounting walks "MSG-LEN SP MSG" frames. Heroku packs them
// back to back with no separator; a stray LF between frames (some
// senders add one) is tolerated.
func scanOctetCounting(br *bufio.Reader, maxFrame int, deliver func([]byte), fail func(Frame, error)) {
	for {
		// Skip an optional separator byte between frames.
		for {
			b, err := br.ReadByte()
			if err != nil {
				return
			}
			if b == '\n' || b == '\r' || b == ' ' {
				continue
			}
			_ = br.UnreadByte()
			break
		}

		lengthField, err := br.ReadBytes(' ')
		if err != nil {
			if len(bytes.TrimSpace(lengthField)) == 0 {
				return // clean end between frames
			}
			fail(Frame{}, fmt.Errorf("syslog framing: unterminated length %q", bytes.TrimSpace(lengthField)))
			return
		}
		n, err := strconv.Atoi(string(bytes.TrimSpace(lengthField)))
		if err != nil || n <= 0 {
			fail(Frame{}, fmt.Errorf("syslog framing: invalid length %q", bytes.TrimSpace(lengthField)))
			return
		}
		if n > maxFrame {
			fail(Frame{}, fmt.Errorf("syslog framing: frame length %d exceeds max %d", n, maxFrame))
			return
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			fail(Frame{}, fmt.Errorf("syslog framing: short read on %d-byte frame: %w", n, err))
			return
		}
		deliver(buf)
	}
}

// scanLF walks LF-delimited frames. Blank lines are skipped; a CR
// before the LF is trimmed.
func scanLF(br *bufio.Reader, maxFrame int, deliver func([]byte), fail func(Frame, error)) {
	for {
		line, err := br.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return
		}
		frame := bytes.TrimRight(line, "\n")
		frame = bytes.TrimRight(frame, "\r")
		if len(bytes.TrimSpace(frame)) > 0 {
			if len(frame) > maxFrame {
				fail(Frame{}, fmt.Errorf("syslog framing: frame length %d exceeds max %d", len(frame), maxFrame))
			} else {
				deliver(frame)
			}
		}
		if err == io.EOF {
			return
		}
	}
}
