// Package logtest captures what code logs, for tests that assert on it.
package logtest

import (
	"bytes"
	"io"
	"log"
	"log/slog"

	weftsdk "github.com/w3ft-net/weft-go"
)

// CaptureInto points every log line at w (JSON, with the ctx's trace ID as
// trace_id) and returns the function that puts the previous destination back.
func CaptureInto(w io.Writer) (restore func()) {
	prev, out, flags := slog.Default(), log.Writer(), log.Flags()
	weftsdk.InitLogging("", w)
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
