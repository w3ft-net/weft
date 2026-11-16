package syslog

import (
	"bytes"
	"errors"
	"strings"

	"github.com/w3ft-net/weft/sources"
)

// RenderProducer implements StreamProducer for render.com's TLS syslog log
// streams. Field mapping and the token's place on the
// wire confirmed against a live wire capture 2026-09-02, not from Render's docs
// alone.
type RenderProducer struct {
	sources.Push
	// ListenPort is the port its listener binds; 0 lets the system choose.
	ListenPort int
}

func (RenderProducer) Name() string { return "render" }

func (p RenderProducer) Port() int { return p.ListenPort }

// Token reads the literal cleartext "TOKEN SP" prefix Render repeats on every
// line of the stream. A bare RFC 5424 frame (it starts with '<') is what Render
// sends when no Token is configured in the log stream, so it is refused rather
// than guessed at, as every other ingest adapter does for a missing credential.
func (RenderProducer) Token(line []byte) (string, []byte, error) {
	trimmed := bytes.TrimRight(line, "\r\n")
	if len(trimmed) == 0 || trimmed[0] == '<' {
		return "", nil, errors.New("no token prefix")
	}
	sp := bytes.IndexByte(trimmed, ' ')
	if sp <= 0 {
		return "", nil, errors.New("malformed first line")
	}
	return string(trimmed[:sp]), line[sp+1:], nil
}

// MapFields maps HOSTNAME (the service slug) to Service, and APP-NAME —
// falling back to MSGID, which the capture showed carries the identical
// value — split on the *last* "-" to Kind/Instance: Render's kind tokens
// (web, cron, bld, ...) never contain a hyphen, so "web-9nk6q" ->
// ("web", "9nk6q"), "cron-stqq5" -> ("cron", "stqq5").
//
// Render's own connectivity-test ping arrives with HOSTNAME "render" and
// is dropped (ok=false) rather than forwarded as a fake service.
func (RenderProducer) MapFields(f Frame) (service, kind, instance string, ok bool) {
	if f.Hostname == "" || f.Hostname == "render" {
		return "", "", "", false
	}
	appOrMsg := f.AppName
	if appOrMsg == "" {
		appOrMsg = f.MsgID
	}
	kind, instance, ok = SplitKindInstance(appOrMsg)
	if !ok {
		return "", "", "", false
	}
	return f.Hostname, kind, instance, true
}

// SplitKindInstance splits "web-9nk6q" into ("web", "9nk6q") on the last
// "-". ok is false when there's nothing to split (empty or no hyphen) —
// that shape hasn't been observed and isn't safe to guess at. Render's
// metrics stream ids split the same way ("srv-<id>-j9fbv" gives instance
// "j9fbv"), so the OTLP adapter shares this to keep logs and metrics on one
// $host.
func SplitKindInstance(s string) (kind, instance string, ok bool) {
	i := strings.LastIndexByte(s, '-')
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}
