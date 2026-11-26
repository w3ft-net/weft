package shell

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ExecClient runs pipelines against the API server's /api/shell/exec stream.
// It is the one transport the CLI and the browser shell share: the CLI builds
// it over HTTPCoordinator, the browser over fetch (Go's net/http under
// GOOS=js).
type ExecClient struct {
	// BaseURL is the API server, without a trailing slash.
	BaseURL string
	// Headers returns the request's auth and client headers. It runs on every
	// request, so a token that changes between calls is picked up.
	Headers func() map[string]string
	// Transport carries the requests. It has no client-side deadline: how long
	// a query may run is the server's to say, and a follow lives until its
	// context is cancelled.
	Transport ExecTransport
	// Unauthorized is the error a 401 becomes; nil leaves it as an HTTP error.
	Unauthorized error
}

// ExecTransport sends one POST and hands back the response without reading
// it. The CLI's is net/http; the browser's is fetch, which keeps net/http out
// of the wasm binary.
type ExecTransport interface {
	Post(ctx context.Context, url string, header map[string]string, body []byte) (*ExecResponse, error)
}

// ExecResponse is a response whose body is still streaming; the caller closes
// Body.
type ExecResponse struct {
	Status int
	Body   io.ReadCloser
}

// Query runs pipeline once and returns what the server streamed back.
func (c *ExecClient) Query(ctx context.Context, pipeline, rangeSpec string, dirStack []string) (*QueryResult, error) {
	resp, err := c.post(ctx, map[string]any{
		"pipeline": pipeline,
		"follow":   false,
		"range":    rangeSpec,
		"dirstack": dirStack,
	})
	if err != nil {
		if isTimeout(err) {
			return nil, errQueryTimedOut
		}
		return nil, fmt.Errorf("query: %w", err)
	}
	defer resp.Body.Close()
	if err := c.statusError(resp); err != nil {
		return nil, err
	}

	res := &QueryResult{}
	err = c.readEvents(resp.Body, func(ev map[string]any) (bool, error) {
		switch ev["type"] {
		case "line":
			if d, ok := ev["data"].(map[string]any); ok {
				raw, _ := d["raw"].(string)
				res.Rows = append(res.Rows, Row{Raw: raw})
			}
		case "stderr":
			// Red, so it reads as an error among the rows.
			if text, ok := ev["text"].(string); ok {
				res.Text += "\033[38;5;1m" + text + "\033[0m"
			}
		case "notice":
			res.Notices = append(res.Notices, QueryResultNotice{
				Key:   stringOf(ev["key"]),
				Kind:  stringOf(ev["kind"]),
				Title: stringOf(ev["title"]),
				Body:  stringOf(ev["body"]),
			})
		case "cwd":
			// The post-run directory stack (index 0 == cwd), so the client
			// can show the cwd a shipped cd/pushd established.
			if raw, ok := ev["stack"].([]any); ok {
				stack := make([]string, 0, len(raw))
				for _, v := range raw {
					stack = append(stack, stringOf(v))
				}
				res.FinalDirStack = stack
			}
		case "error":
			if msg := eventMessage(ev); msg != "" {
				return true, fmt.Errorf("%s", msg)
			}
		case "done":
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Follow streams pipeline, calling onRow for each row, until the stream ends
// or ctx is cancelled.
func (c *ExecClient) Follow(ctx context.Context, pipeline, rangeSpec string, onRow func(Row)) error {
	resp, err := c.post(ctx, map[string]any{
		"pipeline": pipeline,
		"follow":   true,
		"range":    rangeSpec,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := c.statusError(resp); err != nil {
		return err
	}
	return c.readEvents(resp.Body, func(ev map[string]any) (bool, error) {
		switch ev["type"] {
		case "line":
			if d, ok := ev["data"].(map[string]any); ok {
				raw, _ := d["raw"].(string)
				onRow(Row{Raw: raw})
			}
		case "error":
			if msg := eventMessage(ev); msg != "" {
				return true, fmt.Errorf("%s", msg)
			}
		}
		return false, nil
	})
}

func (c *ExecClient) post(ctx context.Context, body map[string]any) (*ExecResponse, error) {
	b, _ := json.Marshal(body)
	hdr := map[string]string{"Content-Type": "application/json"}
	if c.Headers != nil {
		for k, v := range c.Headers() {
			hdr[k] = v
		}
	}
	return c.Transport.Post(ctx, c.BaseURL+"/api/shell/exec", hdr, b)
}

// statusError turns a response that is not a 200 stream into an error.
func (c *ExecClient) statusError(resp *ExecResponse) error {
	switch {
	case resp.Status == 200:
		return nil
	case resp.Status == 426: // Upgrade Required
		return formatUpgradeRequired(resp.Body)
	case resp.Status == 401 && c.Unauthorized != nil:
		io.Copy(io.Discard, resp.Body)
		return c.Unauthorized
	}
	msg, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("HTTP %d: %s", resp.Status, strings.TrimSpace(string(msg)))
}

// readEvents calls handle with each SSE event until it asks to stop or the
// stream ends.
func (c *ExecClient) readEvents(r io.Reader, handle func(map[string]any) (stop bool, err error)) error {
	// Real log lines run past bufio's 64KB default (stack traces, JSON
	// payloads); the ceiling is lifted so a long one does not end the stream.
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		if stop, err := handle(ev); err != nil || stop {
			return err
		}
	}
	return scanner.Err()
}

// eventMessage is an error event's text: servers have sent it as "message"
// and as "text".
func eventMessage(ev map[string]any) string {
	if m, ok := ev["message"].(string); ok && m != "" {
		return m
	}
	m, _ := ev["text"].(string)
	return m
}

// stringOf coerces a JSON-decoded value to a string, "" for anything else.
func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// errQueryTimedOut is what a user sees when a query's connection times out
// on the client's side. It says what to do about it, without naming anything
// of ours.
var errQueryTimedOut = errors.New("query timed out — narrow the time range, filter earlier in the pipeline, or run `explain` on it to see where the time goes")

// formatUpgradeRequired turns the server's 426 JSON body into a
// human-friendly error. The server response shape is
// {"error":"client too old","running":"…","required":"…","upgrade":"…"} —
// surface the required version + the upgrade hint so the user knows
// exactly what to do without dumping a stack trace.
func formatUpgradeRequired(body io.Reader) error {
	var payload struct {
		Error    string `json:"error"`
		Running  string `json:"running"`
		Required string `json:"required"`
		Upgrade  string `json:"upgrade"`
	}
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		return fmt.Errorf("this weft CLI is too old for the server — run `weft update`")
	}
	hint := payload.Upgrade
	if hint == "" {
		hint = "run `weft update`"
	}
	return fmt.Errorf("this weft CLI (%s) is too old — server requires %s. %s",
		payload.Running, payload.Required, hint)
}

// isTimeout reports whether err is a deadline expiring, whether the client's
// own or a context's.
func isTimeout(err error) bool {
	var te interface{ Timeout() bool }
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &te) && te.Timeout())
}
