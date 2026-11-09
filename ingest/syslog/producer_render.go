package syslog

import "strings"

// RenderProducer implements Producer for render.com's TLS syslog log
// streams (docs/ingestion-syslog.md). Field mapping confirmed against a
// live wire capture 2026-09-02, not from Render's docs alone.
type RenderProducer struct{}

func (RenderProducer) Name() string { return "render" }

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
