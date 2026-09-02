package syslog

// Producer differentiates one syslog-speaking platform's frame shape from
// another. SyslogServer (api/syslog_server.go) is otherwise fully
// generic — TLS termination, token auth, LF framing, RFC5424 parsing —
// and knows nothing platform-specific. Each SyslogServer instance is
// constructed with exactly one Producer; it owns one NLB listener/port,
// so it never needs to guess which platform a connection is from (see
// docs/ingestion-syslog.md, "Producer identity comes from the listener,
// never from content"). Tokens are validated scoped to the same
// Name(), so a token minted for a different producer can't be replayed
// against this listener even if it leaked.
//
// A second syslog-speaking platform (Fly.io, Railway, ...) is a new file
// implementing this interface plus a new NLB listener/cert/port — not
// new SyslogServer code.
type Producer interface {
	// Name identifies this producer for token scoping
	// (store.ValidateAgentToken) and for `weft token create --for
	// <name>`. Stable — changing it invalidates every token minted for
	// this producer.
	Name() string

	// MapFields turns one parsed Frame's HOSTNAME/APP-NAME/MSGID into
	// weft's Service/Kind/Instance triple. ok=false drops the frame
	// (e.g. Render's own connectivity-test ping) without forwarding it.
	MapFields(f Frame) (service, kind, instance string, ok bool)
}
