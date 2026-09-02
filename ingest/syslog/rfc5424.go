// Package syslog adapts github.com/leodido/go-syslog to the narrow
// shape weft's platform-log ingest paths need. The Heroku HTTPS drain
// adapter (api.HerokuAdapter) is the first consumer; the Render TLS
// syslog listener (docs/ingestion-syslog.md) is the second. Frame is a flat
// value with weft-friendly zero values ("" for an absent header, a
// best-effort now for an absent timestamp); the RFC 5424 grammar and
// RFC 6587 framing are the library's problem, not ours.
package syslog

import (
	"time"

	"github.com/leodido/go-syslog/v4/rfc5424"
)

// Frame is the subset of an RFC 5424 syslog message a weft ingest
// adapter consumes.
type Frame struct {
	Priority int
	Facility int // Priority >> 3
	Severity int // Priority & 7

	// Timestamp is the message TIMESTAMP, or time.Now().UTC() when the
	// field is absent or unparseable.
	Timestamp time.Time

	// Header fields. An RFC 5424 NILVALUE ("-") arrives as "".
	Hostname string
	AppName  string
	ProcID   string
	MsgID    string

	// StructuredData holds every SD-ELEMENT keyed by SD-ID, each a map
	// of PARAM-NAME to its unescaped PARAM-VALUE. Nil when the frame
	// carries "-". Heroku never sends any; Render may, depending on
	// whether it recognises the endpoint.
	StructuredData map[string]map[string]string

	Message string
}

// SD returns one structured-data parameter and whether it was present.
// Convenience for the "one known SD-ID, one known key" lookup an
// adapter's auth path does.
func (f Frame) SD(id, key string) (string, bool) {
	el, ok := f.StructuredData[id]
	if !ok {
		return "", false
	}
	v, ok := el[key]
	return v, ok
}

// fromRFC5424 flattens a parsed library message into a Frame. Every
// library field is an optional pointer; a nil one becomes the Go zero
// value, except Timestamp which falls back to now so a row always has a
// usable time.
func fromRFC5424(m *rfc5424.SyslogMessage) Frame {
	f := Frame{Timestamp: time.Now().UTC()}
	if m == nil {
		return f
	}
	if m.Priority != nil {
		f.Priority = int(*m.Priority)
	}
	if m.Facility != nil {
		f.Facility = int(*m.Facility)
	}
	if m.Severity != nil {
		f.Severity = int(*m.Severity)
	}
	if m.Timestamp != nil {
		f.Timestamp = *m.Timestamp
	}
	f.Hostname = deref(m.Hostname)
	f.AppName = deref(m.Appname)
	f.ProcID = deref(m.ProcID)
	f.MsgID = deref(m.MsgID)
	f.Message = deref(m.Message)
	if m.StructuredData != nil {
		f.StructuredData = *m.StructuredData
	}
	return f
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
