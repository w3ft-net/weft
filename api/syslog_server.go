package api

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"

	ingsyslog "github.com/graybeardcorp/weft/ingest/syslog"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
)

// syslogIdleTimeout closes a connection that goes this long without a
// complete frame. Render doesn't document a keepalive cadence; this is
// generous enough to tolerate a quiet service while still reclaiming a
// half-open TCP connection (a dropped NLB target, a client that
// vanished without a FIN) in bounded time. Reset on every frame, not
// just at connect.
const syslogIdleTimeout = 5 * time.Minute

// SyslogServer is a TCP syslog listener for exactly one
// ingest/syslog.Producer. TLS terminates upstream at the NLB (see
// docs/ingestion-syslog.md), so this only ever sees
// plaintext TCP. Producer identity comes from which port a connection
// lands on, decided by the NLB listener before any bytes arrive — this
// server never guesses. Auth, LF framing, and RFC5424 parsing are all
// generic; Producer.MapFields is the only platform-specific step. A
// second syslog-speaking platform gets its own SyslogServer instance on
// its own port, not new code here.
type SyslogServer struct {
	store       *store.Store
	coordClient pb.WeftCoordinatorClient
	producer    ingsyslog.Producer
	port        int

	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

// NewSyslogServer constructs a SyslogServer bound to one Producer and
// port, reusing this Server's store and coordinator client. Call Start
// to bind the listener and begin accepting connections.
func (s *Server) NewSyslogServer(producer ingsyslog.Producer, port int) *SyslogServer {
	return &SyslogServer{
		store:       s.store,
		coordClient: s.coordClient,
		producer:    producer,
		port:        port,
		conns:       make(map[net.Conn]struct{}),
	}
}

// Start binds the listener and begins accepting connections in the
// background, returning once the listener is bound so a caller (e.g.
// cmd/api/main.go) can fail fast on a port conflict instead of
// discovering it asynchronously. Close (or ctx cancellation) stops the
// accept loop and drains in-flight connections.
func (ss *SyslogServer) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", ss.port))
	if err != nil {
		return fmt.Errorf("syslog[%s]: listen on :%d: %w", ss.producer.Name(), ss.port, err)
	}
	ss.ln = ln

	go func() {
		<-ctx.Done()
		ss.Close()
	}()

	ss.wg.Add(1)
	go ss.acceptLoop(ctx)
	return nil
}

// Close stops accepting new connections and closes every connection
// currently in flight — a graceful-drain step. A connection mid-frame
// simply loses its next read; the rows already parsed from it were
// already sent to the coordinator one frame at a time, so nothing
// beyond the in-flight frame is lost. Blocks until every connection
// goroutine has returned.
func (ss *SyslogServer) Close() {
	ss.mu.Lock()
	if ss.ln != nil {
		ss.ln.Close()
	}
	for c := range ss.conns {
		c.Close()
	}
	ss.mu.Unlock()
	ss.wg.Wait()
}

func (ss *SyslogServer) acceptLoop(ctx context.Context) {
	defer ss.wg.Done()
	for {
		conn, err := ss.ln.Accept()
		if err != nil {
			return // listener closed via Close(), or a fatal accept error
		}
		ss.trackConn(conn, true)
		ss.wg.Add(1)
		go func() {
			defer ss.wg.Done()
			defer ss.trackConn(conn, false)
			ss.handleConn(ctx, conn)
		}()
	}
}

func (ss *SyslogServer) trackConn(conn net.Conn, add bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if add {
		ss.conns[conn] = struct{}{}
	} else {
		delete(ss.conns, conn)
	}
}

// handleConn owns one Render (or other producer) connection end to
// end: read the token off the first line, validate it scoped to this
// server's producer, open one coordinator IngestStream for the life of
// the connection, and forward every frame that Producer.MapFields
// accepts. See docs/ingestion-syslog.md, "Auth model", for the
// leading-"TOKEN SP" wire shape this decodes.
func (ss *SyslogServer) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	name := ss.producer.Name()
	remote := conn.RemoteAddr()
	br := bufio.NewReaderSize(conn, 4096)

	conn.SetReadDeadline(time.Now().Add(syslogIdleTimeout))
	firstLine, err := br.ReadBytes('\n')
	if len(firstLine) == 0 {
		return // closed before sending anything
	}
	trimmed := bytes.TrimRight(firstLine, "\r\n")
	// No leading "TOKEN SP": Render sends a bare RFC5424 frame (starts
	// with '<') when no Token is configured in the log stream. Reject
	// outright rather than guess a namespace — matches every other
	// ingest adapter's behaviour on a missing credential.
	if len(trimmed) == 0 || trimmed[0] == '<' {
		log.Printf("syslog[%s]: rejecting %s: connection has no token prefix", name, remote)
		return
	}
	sp := bytes.IndexByte(trimmed, ' ')
	if sp <= 0 {
		log.Printf("syslog[%s]: rejecting %s: malformed first line", name, remote)
		return
	}
	token := string(trimmed[:sp])
	tokenPrefix := append([]byte(nil), trimmed[:sp+1]...) // "TOKEN " including the space

	auth, err := ss.store.ValidateAgentTokenForProducer(token, name)
	if err != nil {
		log.Printf("syslog[%s]: token validation error from %s: %v", name, remote, err)
		return
	}
	if auth == nil {
		log.Printf("syslog[%s]: rejecting %s: invalid, revoked, or wrong-producer token", name, remote)
		return
	}

	// Same bearer token in gRPC metadata so the coordinator's own
	// authenticateStream re-validates independently — this server's
	// producer-scoped check is an extra gate, not a replacement for it.
	streamCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	stream, err := ss.coordClient.IngestStream(streamCtx)
	if err != nil {
		log.Printf("syslog[%s]: open ingest stream for %s: %v", name, auth.Namespace, err)
		return
	}

	r := &syslogConnReader{
		br:      br,
		prefix:  tokenPrefix,
		pending: append(trimmed[sp+1:], '\n'),
	}
	streamBroken := false
	ingsyslog.ParseStream(r, ingsyslog.NonTransparent, ingsyslog.DefaultMaxFrame, func(f ingsyslog.Frame, ferr error) {
		if streamBroken {
			return
		}
		// Any activity — including a frame that fails to parse — counts
		// as liveness; only a truly silent connection should idle out.
		conn.SetReadDeadline(time.Now().Add(syslogIdleTimeout))
		if ferr != nil {
			return // skip the bad frame, keep the connection — matches HerokuAdapter's tolerance
		}
		service, kind, instance, ok := ss.producer.MapFields(f)
		if !ok {
			return // e.g. Render's own connectivity-test ping
		}
		row := &pb.LogRow{
			Service:  service,
			Kind:     kind,
			Instance: instance,
			Raw:      f.Message,
			TsUnixNs: f.Timestamp.UnixNano(),
		}
		if err := stream.Send(row); err != nil {
			log.Printf("syslog[%s]: send row for %s: %v", name, auth.Namespace, err)
			streamBroken = true
			// Unblock the next br.Read so ParseStream returns promptly
			// instead of waiting out the full idle timeout.
			conn.Close()
		}
	})

	if _, err := stream.CloseAndRecv(); err != nil && !streamBroken {
		log.Printf("syslog[%s]: close ingest stream for %s: %v", name, auth.Namespace, err)
	}
}

// syslogConnReader turns a producer's "TOKEN SP <frame>\n" per-line
// framing into plain "<frame>\n" framing so the shared
// ingest/syslog.ParseStream (NonTransparent/LF mode) can be reused
// unmodified. Render's wire capture confirmed the token repeats on
// every line, not just the first — see docs/ingestion-syslog.md,
// "Wire-capture first, always".
type syslogConnReader struct {
	br      *bufio.Reader
	prefix  []byte // "TOKEN ", including the trailing space
	pending []byte // next bytes to hand out; primed with the first line's stripped body
}

func (r *syslogConnReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		line, err := r.br.ReadBytes('\n')
		if len(line) > 0 {
			r.pending = bytes.TrimPrefix(line, r.prefix)
		}
		if err != nil {
			if len(r.pending) == 0 {
				return 0, err
			}
			break
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
