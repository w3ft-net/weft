package weftlog

import (
	"bytes"
	"io"
	"log"
	"log/slog"
)

// CaptureInto points every log line at w, as Init does, and returns the
// function that puts the previous destination back. For tests that assert on
// what a code path logged.
func CaptureInto(w io.Writer) (restore func()) {
	prev, out, flags := slog.Default(), log.Writer(), log.Flags()
	Init(w)
	return func() {
		slog.SetDefault(prev)
		log.SetOutput(out)
		log.SetFlags(flags)
	}
}

// Capture returns everything logged while fn ran.
func Capture(fn func()) string {
	var buf bytes.Buffer
	defer CaptureInto(&buf)()
	fn()
	return buf.String()
}
