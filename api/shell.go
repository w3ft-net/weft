package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

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
//	{"type":"output","text":"..."}          — raw text output
//	{"type":"row","data":{...log fields}}   — a structured log row
//	{"type":"error","message":"..."}        — terminal error
//	{"type":"done"}                         — stream complete (non-follow mode)
func (s *Server) handleShellExec(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r)
	if s.store != nil {
		if sub, _ := s.store.ActiveSubscription(claims.CustomerName); sub == nil {
			http.Error(w, "no active subscription", http.StatusPaymentRequired)
			return
		}
	}

	if s.coordClient == nil {
		http.Error(w, "coordinator unavailable", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Pipeline string `json:"pipeline"`
		Follow   bool   `json:"follow"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Pipeline == "" {
		http.Error(w, "pipeline required", http.StatusBadRequest)
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
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	stream, err := s.coordClient.Query(ctx, &pb.CoordinatorQueryRequest{
		Pipeline: req.Pipeline,
		Follow:   req.Follow,
	})
	if err != nil {
		writeSSEEvent(w, map[string]any{"type": "error", "message": err.Error()})
		flusher.Flush()
		return
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Client disconnected — not an error worth logging loudly.
			if ctx.Err() != nil {
				return
			}
			log.Printf("shell exec stream error: %v", err)
			writeSSEEvent(w, map[string]any{"type": "error", "message": "stream error"})
			flusher.Flush()
			return
		}

		if resp.Text != "" {
			writeSSEEvent(w, map[string]any{"type": "output", "text": resp.Text})
		}

		for _, row := range resp.Rows {
			writeSSEEvent(w, map[string]any{
				"type": "row",
				"data": logRowToMap(row),
			})
		}

		flusher.Flush()
	}

	if !req.Follow {
		writeSSEEvent(w, map[string]any{"type": "done"})
		flusher.Flush()
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

// logRowToMap converts a LogRow proto to a plain map for JSON serialisation.
func logRowToMap(row *pb.LogRow) map[string]any {
	m := map[string]any{
		"line_num":  row.LineNum,
		"raw":       row.Raw,
		"app":       row.App,
		"level":     row.Level,
		"ts_unix_ns": row.TsUnixNs,
		"instance":  row.Instance,
	}
	if len(row.PayloadJson) > 0 {
		var payload any
		if err := json.Unmarshal(row.PayloadJson, &payload); err == nil {
			m["payload"] = payload
		}
	}
	if len(row.Fields) > 0 {
		m["fields"] = row.Fields
	}
	return m
}
