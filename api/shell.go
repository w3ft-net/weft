package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"google.golang.org/grpc/status"

	"github.com/kevlarcon/weft/internal/timerange"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// sseKeepaliveInterval is how often handleShellExec writes a heartbeat
// comment to an idle SSE stream. Must be shorter than the shortest idle
// timeout imposed by any intermediate proxy. AWS ALB's default is 60s,
// nginx's default keepalive_timeout is 75s, and browsers time out at 30s
// for some adaptive-bitrate flows — 20s gives a comfortable 3x margin.
//
// Overridden by tests via the unexported setter to exercise the
// keepalive path in milliseconds instead of seconds.
var sseKeepaliveInterval = 20 * time.Second

// handleShellExec executes a weft pipeline and streams results back as
// Server-Sent Events (SSE). The frontend uses EventSource or fetch with
// ReadableStream to consume the stream.
//
// Request body:
//
//	{"pipeline": "cat vimgym.log | grep ERROR", "follow": false}
//
// Each SSE event is JSON in one of two shapes:
//
//	{"type":"line","data":{"raw":"..."}}   — a log line
//	{"type":"error","message":"..."}       — terminal error
//	{"type":"done"}                        — stream complete (non-follow mode)
//
// Between events, the handler writes SSE comment lines (`: keepalive`)
// at sseKeepaliveInterval so intermediate proxies (ALB, nginx, browser
// HTTP/2 frame timers) don't drop the connection during quiet periods.
// Clients ignore comment lines per the SSE spec.
func (s *Server) handleShellExec(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if sub, _ := s.store.ActiveSubscription(session.ActiveCustomerID); sub == nil {
		http.Error(w, "no active subscription", http.StatusPaymentRequired)
		return
	}

	var req struct {
		Pipeline string `json:"pipeline"`
		Follow   bool   `json:"follow"`
		// Range is an unresolved time-window spec ("1d..", "30m..10m",
		// "2026-04-01..2026-04-12", "05-14T09:00..05-14T17:00"). The
		// server resolves it on every request against its own clock so a
		// long-lived shell pane never queries against a stale window. An
		// empty Range is rejected — defaults are the client's job.
		Range string `json:"range"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Pipeline == "" {
		http.Error(w, "pipeline required", http.StatusBadRequest)
		return
	}

	if req.Range == "" {
		http.Error(w, "range required", http.StatusBadRequest)
		return
	}
	start, end, err := timerange.ParseRange(req.Range)
	if err != nil {
		http.Error(w, fmt.Sprintf("range: %v", err), http.StatusBadRequest)
		return
	}

	// Set up SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering if present

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Use the request context so the stream is cancelled when the client disconnects.
	// coordCtx forwards the caller's bearer token as outgoing gRPC metadata.
	ctx, cancel := context.WithCancel(s.coordCtx(r))
	defer cancel()

	stream, err := s.coordClient.Execute(ctx, &pb.CoordinatorExecuteRequest{
		Pipeline:    req.Pipeline,
		Follow:      req.Follow,
		Customer:    session.ActiveCustomerName,
		UserId:      session.UserID,
		TimeStartNs: start.UnixNano(),
		TimeEndNs:   end.UnixNano(),
	})
	if err != nil {
		writeSSEEvent(w, map[string]any{"type": "error", "message": err.Error()})
		flusher.Flush()
		return
	}

	// Receive on a goroutine; main loop multiplexes with the keepalive
	// ticker so idle streams still write to the response writer on a
	// predictable cadence. http.ResponseWriter isn't safe for concurrent
	// writes — the goroutine does only Recv, never Write.
	type recvResult struct {
		resp *pb.CoordinatorExecuteResponse
		err  error
	}
	recvCh := make(chan recvResult)
	go func() {
		defer close(recvCh)
		for {
			resp, err := stream.Recv()
			recvCh <- recvResult{resp, err}
			if err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(sseKeepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			// SSE spec: a line starting with ':' is a comment, ignored
			// by clients. Writing one keeps the TCP connection active.
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()

		case rr, ok := <-recvCh:
			if !ok {
				// Goroutine exited with an error already forwarded on
				// the previous iteration; done.
				return
			}
			if rr.err == io.EOF {
				// Pipeline finished cleanly. If the user is in the
				// shell-* phase of onboarding and the pipeline matches
				// the current lesson's predicate, advance the step
				// and emit the next lesson's prompt so the UI banner
				// can update without waiting for a /me poll.
				if newStep, prompt, advanced := s.maybeAdvanceShellLesson(session, req.Pipeline); advanced {
					writeSSEEvent(w, map[string]any{
						"type":   "tour_advance",
						"step":   newStep,
						"prompt": prompt,
					})
				}
				if !req.Follow {
					writeSSEEvent(w, map[string]any{"type": "done"})
					flusher.Flush()
				}
				return
			}
			if rr.err != nil {
				if ctx.Err() != nil {
					// Client disconnected — not an error worth logging loudly.
					return
				}
				log.Printf("shell exec stream error: %v", rr.err)
				msg := rr.err.Error()
				if s, ok := status.FromError(rr.err); ok {
					// Strip the "rpc error: code = ... desc = " envelope.
					msg = s.Message()
				}
				writeSSEEvent(w, map[string]any{"type": "error", "message": msg})
				flusher.Flush()
				return
			}

			// Notices come on the first response in a stream — emit
			// them before any output/rows so the user sees the heads-up
			// before the actual command output renders.
			for _, n := range rr.resp.Notices {
				writeSSEEvent(w, map[string]any{
					"type":  "notice",
					"key":   n.Key,
					"kind":  n.Kind,
					"title": n.Title,
					"body":  n.Body,
				})
			}
			for _, line := range rr.resp.Lines {
				writeSSEEvent(w, map[string]any{
					"type": "line",
					"data": map[string]any{
						"raw": line.Raw,
					},
				})
			}
			if rr.resp.Stderr != "" {
				writeSSEEvent(w, map[string]any{"type": "stderr", "text": rr.resp.Stderr})
			}
			flusher.Flush()
		}
	}
}

// writeSSEEvent writes a single SSE event with a JSON payload.
func writeSSEEvent(w http.ResponseWriter, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}
