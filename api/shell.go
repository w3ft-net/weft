package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/graybeardcorp/weft/coordinator/onboarding"
	"github.com/graybeardcorp/weft/internal/timerange"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
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
//	{"type":"cwd","cwd":"...","stack":[...]} — post-run dirstack (non-follow);
//	                                          sent just before "done"
//	{"type":"done"}                        — stream complete (non-follow mode)
//
// Between events, the handler writes SSE comment lines (`: keepalive`)
// at sseKeepaliveInterval so intermediate proxies (ALB, nginx, browser
// HTTP/2 frame timers) don't drop the connection during quiet periods.
// Clients ignore comment lines per the SSE spec.
func (s *Server) handleShellExec(w http.ResponseWriter, r *http.Request) {
	session := GetSession(r)
	if sub, _ := s.store.ActiveSubscription(session.ActiveOrgID); sub == nil {
		http.Error(w, "no active subscription", http.StatusPaymentRequired)
		return
	}

	var req struct {
		Pipeline string `json:"pipeline"`
		Follow   bool   `json:"follow"`
		// Modal is set by a browser modal fetching its own rows (the trace
		// view). Its pipeline finishing is not the user finishing the lesson.
		Modal bool `json:"modal"`
		// Range is an unresolved time-window spec ("1d..", "30m..10m",
		// "2026-04-01..2026-04-12", "05-14T09:00..05-14T17:00"). The
		// server resolves it on every request against its own clock so a
		// long-lived shell pane never queries against a stale window. An
		// empty Range is rejected — defaults are the client's job.
		Range string `json:"range"`
		// DirStack is the delegated client's cached directory stack
		// (index 0 == cwd, the rest are pushd entries). Ferried to the
		// coordinator as weft-dirstack metadata so the server resolves
		// cd/relative paths against the client's live cwd; the post-run
		// stack returns on the "cwd" SSE event for the client to cache.
		// Empty/omitted behaves like a fresh shell rooted at "/".
		DirStack []string `json:"dirstack"`
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

	// Team-management commands (adduser/deluser/groups/id) are handled
	// in-process — they need the email sender, seat limits, and owner
	// session that live here, not on the coordinator. Everything else
	// forwards to the coordinator below.
	if handled, out, cmdErr := s.maybeHandleTeamCommand(session, req.Pipeline); handled {
		if cmdErr != nil {
			writeSSEEvent(w, map[string]any{"type": "stderr", "text": cmdErr.Error() + "\n"})
		} else if out != "" {
			for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
				writeSSEEvent(w, map[string]any{"type": "line", "data": map[string]any{"raw": line}})
			}
		}
		writeSSEEvent(w, map[string]any{"type": "done"})
		flusher.Flush()
		return
	}

	// Use the request context so the stream is cancelled when the client disconnects.
	// coordCtx forwards the caller's bearer token as outgoing gRPC metadata.
	ctx, cancel := context.WithCancel(s.coordCtx(r))
	defer cancel()

	// Dirstack courier (out to coordinator): ferry the client's cached
	// stack as repeated weft-dirstack metadata, matching what the Execute
	// RPC reads. Each entry is one metadata value; index 0 is cwd.
	for _, d := range req.DirStack {
		ctx = metadata.AppendToOutgoingContext(ctx, "weft-dirstack", d)
	}

	trigger := onboarding.TriggerExec
	if req.Modal {
		trigger = onboarding.TriggerModalFetch
	}

	// Capture the response trailer so the post-run dirstack (weft-final-
	// dirstack) can be surfaced to the client after the stream ends.
	var trailer metadata.MD
	stream, err := s.coordClient.Execute(ctx, &pb.CoordinatorExecuteRequest{
		Pipeline:    req.Pipeline,
		Follow:      req.Follow,
		UserId:      session.UserID,
		TimeStartNs: start.UnixNano(),
		TimeEndNs:   end.UnixNano(),
	}, grpc.Trailer(&trailer))
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
			// A tail -f stream is designed to stay open indefinitely —
			// it only ever ends via client cancellation (this branch),
			// never a server-side io.EOF. Without this, a follow-mode
			// lesson (e.g. the tour's shell-tail-f) could never advance:
			// the client is already gone, so there's no SSE event to
			// write, but the server-side session state still updates,
			// and the next action/`/me` poll picks up the advanced
			// step — same fallback path every other lesson already
			// relies on. Treats "the user stopped a live tail" as
			// completion, same permissive bar (syntactic match + clean
			// execution) every other lesson already uses; there's no
			// way to distinguish a deliberate stop from an accidental
			// disconnect, which is an accepted tradeoff here.
			s.maybeAdvanceShellLesson(session, req.Pipeline, nil, trigger)
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
				if newStep, prompt, advanced := s.maybeAdvanceShellLesson(session, req.Pipeline, nil, trigger); advanced {
					writeSSEEvent(w, map[string]any{
						"type":   "tour_advance",
						"step":   newStep,
						"prompt": prompt,
					})
				}
				if !req.Follow {
					// Dirstack courier (back to client): the coordinator
					// returns the post-run stack as a weft-final-dirstack
					// trailer (valid to read now that Recv hit io.EOF).
					// Surface it so the client caches the cwd the pipeline
					// left behind. index 0 is cwd.
					if stack := trailer.Get("weft-final-dirstack"); len(stack) > 0 {
						writeSSEEvent(w, map[string]any{
							"type":  "cwd",
							"cwd":   stack[0],
							"stack": stack,
						})
					}
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
