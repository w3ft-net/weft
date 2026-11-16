package syslog

import "github.com/w3ft-net/weft/sources"

// StreamProducer is a syslog-speaking platform whose logs arrive on a TLS
// listener of its own. SyslogServer (api/syslog_server.go) is otherwise fully
// generic (TLS terminates upstream, token validation, framing, RFC5424
// parsing) and knows nothing platform-specific. Each SyslogServer instance is
// built over exactly one StreamProducer; it owns one NLB listener and port, so
// it never needs to guess which platform a connection is from: producer
// identity comes from the listener, never from content.
//
// It is also a sources.Producer: the rules of the sources it creates are the
// producer's own, and embedding sources.Push gives the usual ones. A new
// syslog-speaking platform (Fly.io, Railway, ...) is a new file implementing this
// interface plus a new NLB listener, cert and port, registered by name in the
// producer registry.
type StreamProducer interface {
	sources.Producer

	// Name identifies this producer in log lines.
	Name() string

	// Port is the port its listener binds.
	Port() int

	// Token splits one line of the stream into the token the connection
	// authenticates with and the frame behind it. It is called on every line,
	// because a platform may repeat its token on each; the server validates the
	// token once, on the first line. An error rejects the connection.
	Token(line []byte) (token string, frame []byte, err error)

	// MapFields turns one parsed Frame's HOSTNAME/APP-NAME/MSGID into
	// weft's Service/Kind/Instance triple. ok=false drops the frame
	// (e.g. Render's own connectivity-test ping) without forwarding it.
	MapFields(f Frame) (service, kind, instance string, ok bool)
}
