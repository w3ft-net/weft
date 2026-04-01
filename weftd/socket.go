package weftd

import (
	"bufio"
	"log"
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
// If no "app:" prefix is present, the default app name is used.
type SocketListener struct {
	path       string
	defaultApp string
	listener   net.Listener
	lines      chan SocketLine
	done       chan struct{}
}

// SocketLine is a parsed line from the socket with its app name.
type SocketLine struct {
	App  string
	Line string
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
				log.Printf("socket: accept: %v", err)
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

		select {
		case s.lines <- SocketLine{App: app, Line: line}:
		default:
			// Channel full — drop line.
		}
	}
}
