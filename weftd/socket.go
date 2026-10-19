package weftd

import (
	"bufio"
	"context"
	weftsdk "github.com/w3ft-net/weft-go"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// SocketListener accepts log lines on a Unix socket.
// Each connection sends newline-delimited messages in the format:
//
//	app:raw log line
//
// If no "app:" prefix is present, the default app name is used. An SDK with
// an ambient trace ID (request-scoped context, not necessarily present in
// the log text itself) may additionally wrap the raw line in a \x1e
// (ASCII Record Separator) envelope to pass it structurally:
//
//	app:\x1eTRACE_ID\x1eraw log line
//
// \x1e never appears in ordinary log text, so this is unambiguous and fully
// backward compatible — lines with no envelope parse exactly as before. See
// docs/plans/request-tracing.md.
type SocketListener struct {
	path       string
	defaultApp string
	listener   net.Listener
	lines      chan SocketLine
	done       chan struct{}
}

// SocketLine is a parsed line from the socket with its app name and,
// optionally, a structurally-carried trace ID (see SocketListener's doc
// comment on the \x1e envelope).
type SocketLine struct {
	App     string
	TraceID string
	Line    string
}

// NewSocketListener creates and starts a Unix socket listener.
func NewSocketListener(path, defaultApp string) (*SocketListener, error) {
	// Ensure directory exists.
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	// Remove stale socket file.
	os.Remove(path)

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// Make socket world-writable so any app can connect.
	os.Chmod(path, 0666)

	s := &SocketListener{
		path:       path,
		defaultApp: defaultApp,
		listener:   listener,
		lines:      make(chan SocketLine, 4096),
		done:       make(chan struct{}),
	}
	go s.accept()
	return s, nil
}

// Lines returns the channel of received log lines.
func (s *SocketListener) Lines() <-chan SocketLine {
	return s.lines
}

// Close stops the listener.
func (s *SocketListener) Close() {
	close(s.done)
	s.listener.Close()
	os.Remove(s.path)
}

func (s *SocketListener) accept() {
	defer close(s.lines)
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				weftsdk.Infof(context.Background(), "socket: accept: %v", err)
				return
			}
		}
		go s.handleConn(conn)
	}
}

func (s *SocketListener) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	// Allow large lines (1MB).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		// Parse optional "app:message" prefix.
		app := s.defaultApp
		if idx := strings.Index(line, ":"); idx > 0 && idx < 32 && !strings.Contains(line[:idx], " ") {
			app = line[:idx]
			line = line[idx+1:]
		}

		// Parse optional \x1eTRACE_ID\x1e envelope. A second blind
		// colon-scan here would be unsafe — ordinary log lines routinely
		// start with a level word and colon ("info: connection reset"),
		// which would false-positive as a trace-id field.
		traceID := ""
		if strings.HasPrefix(line, "\x1e") {
			if end := strings.IndexByte(line[1:], '\x1e'); end >= 0 {
				traceID = line[1 : 1+end]
				line = line[1+end+1:]
			}
		}

		select {
		case s.lines <- SocketLine{App: app, TraceID: traceID, Line: line}:
		default:
			// Channel full — drop line.
		}
	}
}
