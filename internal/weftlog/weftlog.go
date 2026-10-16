// Package weftlog is how every weft server process writes its log lines.
//
// Lines keep the stdlib logger's shape, "2006/01/02 15:04:05 message", because
// the weft/* sources' schemas (and the span roll-up) are detected from it. What
// changes is that the SDK's slog handler sits in front: a line logged with a
// request's ctx gains "trace_id=<id>" without the call site spelling it, so
// every line a request causes can be found by that one ID.
//
// The stdlib logger is routed through the same handler by Init, so lines it
// still writes (the SDK's own span lines) come out in the same shape.
package weftlog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"
)

// Init makes w the destination of every log line this process writes, whether
// through this package, slog, or the stdlib logger. Call it once, first thing
// in main.
func Init(w io.Writer) {
	slog.SetDefault(NewLogger(w))
}

// NewLogger is a logger writing to w in the legacy line shape, with the trace
// ID of the ctx it is given added to each line.
func NewLogger(w io.Writer) *slog.Logger {
	return slog.New(weftsdk.NewSlogHandler(&lineHandler{w: w, mu: new(sync.Mutex)}))
}

// Infof logs a line, with ctx's trace ID if it carries one.
func Infof(ctx context.Context, format string, args ...any) {
	logf(ctx, slog.LevelInfo, format, args)
}

// Warnf is Infof for a line that reports something wrong but survivable.
func Warnf(ctx context.Context, format string, args ...any) {
	logf(ctx, slog.LevelWarn, format, args)
}

// Errorf is Infof for a line that reports a failure.
func Errorf(ctx context.Context, format string, args ...any) {
	logf(ctx, slog.LevelError, format, args)
}

func logf(ctx context.Context, level slog.Level, format string, args []any) {
	l := slog.Default()
	if !l.Enabled(ctx, level) {
		return
	}
	l.Log(ctx, level, fmt.Sprintf(format, args...))
}

// lineHandler writes "<time> <message> key=value ..." lines. Severity is not
// printed: the legacy lines never carried one, and the sources parse them as is.
type lineHandler struct {
	w      io.Writer
	mu     *sync.Mutex // shared by every handler derived from this one
	prefix string      // group path, "a.b."
	attrs  []slog.Attr
}

func (h *lineHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h *lineHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	b.WriteString(t.Format("2006/01/02 15:04:05 "))
	b.WriteString(r.Message)
	for _, a := range h.attrs {
		writeAttr(&b, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(&b, slog.Attr{Key: h.prefix + a.Key, Value: a.Value})
		return true
	})
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteByte('\n')
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *lineHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append([]slog.Attr(nil), h.attrs...)
	for _, a := range attrs {
		c.attrs = append(c.attrs, slog.Attr{Key: h.prefix + a.Key, Value: a.Value})
	}
	return &c
}

func (h *lineHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.prefix = h.prefix + name + "."
	return &c
}

func writeAttr(b *strings.Builder, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		for _, g := range a.Value.Group() {
			writeAttr(b, slog.Attr{Key: a.Key + "." + g.Key, Value: g.Value})
		}
		return
	}
	b.WriteByte(' ')
	b.WriteString(a.Key)
	b.WriteByte('=')
	v := a.Value.String()
	if v == "" || strings.ContainsAny(v, " \t\n\"=") {
		v = strconv.Quote(v)
	}
	b.WriteString(v)
}
