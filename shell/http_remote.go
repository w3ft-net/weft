//go:build !(js && wasm)

package shell

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// RemoteQueryResult is what the shell gets back from a Query RPC.
type RemoteQueryResult struct {
	Rows    []*pb.ResultRow
	Text    string
	Notices []RemoteNotice
}

// RemoteNotice mirrors the proto Notice message decoded from SSE.
// The shell renders these as a prepended block before the actual
// output (see RenderNotices).
type RemoteNotice struct {
	Key   string
	Kind  string
	Title string
	Body  string
}

// SourceInfo describes a configured log source as returned by
// GET /api/sources. Tags match the api server's JSON response so
// encoding/json decodes cleanly into it.
type SourceInfo struct {
	Namespace    string `json:"namespace"`
	App          string `json:"app"`
	SourceType   string `json:"source_type"`
	LogGroup     string `json:"log_group"`
	Region       string `json:"region"`
	PollInterval string `json:"poll_interval"`
	Enabled      bool   `json:"enabled"`
	LastPollAt   string `json:"last_poll_at"`
	AWSAccountID string `json:"aws_account_id,omitempty"`
}

// TokenInfo describes an agent token as returned by GET /api/tokens.
// The raw token is only present in the POST /api/tokens response; it
// is never re-served by the API. Callers identify a token by ID for
// revoke and display Prefix to humans.
type TokenInfo struct {
	ID        uint   `json:"id"`
	Prefix    string `json:"prefix"`
	Label     string `json:"label"`
	Namespace string `json:"namespace"`
	Status    string `json:"status"` // "active" | "revoked"
	CreatedAt string `json:"created_at"`
}

// HTTPCoordinator talks to the weft API over HTTP instead of gRPC.
// This works through the ALB / public internet.
//
// Two clients by design:
//
//   - client: one-shot RPCs (Query, ListApps, token/namespace CRUD, source
//     management). http.Client.Timeout applies to the whole request
//     including the body read, which is exactly right for a short
//     request-reply exchange — it caps how long we'll wait on an
//     unresponsive server.
//   - streamClient: long-lived SSE streams (Follow). Here the body read
//     is the entire point, so a wall-clock Timeout is wrong: it would
//     guarantee the connection dies N seconds in regardless of whether
//     the user or server wanted it to. Cancellation flows through the
//     caller-supplied ctx instead.
//
// Keep the split explicit so a new endpoint has to pick its intent
// rather than inheriting the wrong timeout by accident.
type HTTPCoordinator struct {
	baseURL      string // e.g., "https://api.w3ft.net"
	token        string // JWT for Authorization header
	org          string // X-Active-Org for the user's currently-selected org
	clientID     string // X-Weft-Client; set by cmd/weft via SetClientID
	client       *http.Client
	streamClient *http.Client
}

func NewHTTPCoordinator(baseURL, token, org string) *HTTPCoordinator {
	return &HTTPCoordinator{
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		token:        token,
		org:          org,
		client:       &http.Client{Timeout: 10 * time.Second},
		streamClient: &http.Client{},
	}
}

// SetClientID sets the X-Weft-Client header value. cmd/weft populates this
// from the build-time ClientID() so the API server can read the binary
// version on every request. Leaving it empty means the request omits the
// header (used by unit tests that don't care about the gate).
func (h *HTTPCoordinator) SetClientID(id string) {
	h.clientID = id
}

// setAuthHeaders applies Authorization + X-Active-Org + X-Weft-Client to
// every outgoing request. Centralized so a new endpoint can't forget any
// of them.
func (h *HTTPCoordinator) setAuthHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+h.token)
	if h.org != "" {
		req.Header.Set("X-Active-Org", h.org)
	}
	if h.clientID != "" {
		req.Header.Set("X-Weft-Client", h.clientID)
	}
}

func (h *HTTPCoordinator) Close() error { return nil }

// Query runs a one-shot pipeline against the given range spec. rangeSpec is
// the un-resolved string the user typed ("1d..", "30m..10m",
// "2026-04-01..2026-04-12"); the API server resolves it against its own clock
// per request. See [[feedback_no_legacy_support]] for why we don't pass
// pre-resolved nanos.
func (h *HTTPCoordinator) Query(pipeline, rangeSpec string) (*RemoteQueryResult, error) {
	body, _ := json.Marshal(map[string]any{
		"pipeline": pipeline,
		"follow":   false,
		"range":    rangeSpec,
	})

	req, err := http.NewRequest("POST", h.baseURL+"/api/shell/exec", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("unauthorized — run `weft login` or check WEFT_TOKEN")
	}
	if resp.StatusCode == http.StatusUpgradeRequired {
		return nil, formatUpgradeRequired(resp.Body)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	return parseSSEResponse(resp.Body)
}

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

func (h *HTTPCoordinator) Follow(ctx context.Context, pipeline, rangeSpec string, onRow func(*pb.ResultRow)) error {
	body, _ := json.Marshal(map[string]any{
		"pipeline": pipeline,
		"follow":   true,
		"range":    rangeSpec,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", h.baseURL+"/api/shell/exec", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	h.setAuthHeaders(req)

	// streamClient has no wall-clock Timeout — the request lives for the
	// duration of the caller's ctx.
	resp, err := h.streamClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUpgradeRequired {
		return formatUpgradeRequired(resp.Body)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	// bufio.NewScanner's default MaxScanTokenSize is 64KB. Real log lines
	// routinely run longer (stack traces, JSON blobs with large payloads);
	// lifting the ceiling to 10MB costs nothing at steady state and
	// avoids silent ErrTooLong terminations mid-stream.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var event map[string]any
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}

		switch event["type"] {
		case "line":
			if rowData, ok := event["data"].(map[string]any); ok {
				onRow(mapToResultRow(rowData))
			}
		case "error":
			if msg, ok := event["message"].(string); ok {
				return fmt.Errorf("%s", msg)
			}
		}
	}
	return scanner.Err()
}

func (h *HTTPCoordinator) ListApps() ([]string, error) {
	req, _ := http.NewRequest("GET", h.baseURL+"/api/apps", nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list apps: HTTP %d", resp.StatusCode)
	}

	var apps []string
	json.NewDecoder(resp.Body).Decode(&apps)
	return apps, nil
}

// Client returns nil — HTTP mode doesn't have a gRPC client.
func (h *HTTPCoordinator) Client() pb.WeftCoordinatorClient { return nil }

// NamespaceInfo holds namespace metadata.
type NamespaceInfo struct {
	Name string `json:"name"`
	ID   uint   `json:"id"`
}

func (h *HTTPCoordinator) ListNamespaces() ([]NamespaceInfo, error) {
	req, _ := http.NewRequest("GET", h.baseURL+"/api/namespaces", nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var result struct {
		Namespaces []NamespaceInfo `json:"namespaces"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Namespaces, nil
}

func (h *HTTPCoordinator) CreateNamespace(name string) (*NamespaceInfo, error) {
	body, _ := json.Marshal(map[string]string{"name": name})
	req, _ := http.NewRequest("POST", h.baseURL+"/api/namespaces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var ns NamespaceInfo
	json.NewDecoder(resp.Body).Decode(&ns)
	return &ns, nil
}

func (h *HTTPCoordinator) CreateToken(customer, label, namespace, env string) (string, error) {
	body, _ := json.Marshal(map[string]string{"label": label, "namespace": namespace, "env": env})
	req, _ := http.NewRequest("POST", h.baseURL+"/api/tokens", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var result struct{ Token string `json:"token"` }
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Token, nil
}

func (h *HTTPCoordinator) ListTokens(customer string) ([]TokenInfo, error) {
	req, _ := http.NewRequest("GET", h.baseURL+"/api/tokens", nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var tokens []TokenInfo
	json.NewDecoder(resp.Body).Decode(&tokens)
	return tokens, nil
}

func (h *HTTPCoordinator) RevokeToken(id uint) error {
	req, _ := http.NewRequest("DELETE", fmt.Sprintf("%s/api/tokens/%d", h.baseURL, id), nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// stringOf coerces an `any` from a JSON-decoded map into a string,
// returning "" for any non-string or nil value. Used by the SSE
// notice parser where the protocol guarantees string fields but the
// generic decoder hands back interface{}.
func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// RenderNotice formats a single Notice as a multi-line block ready
// to be printed before a command's output. Title is highlighted via
// ANSI color keyed on Kind: success green, warning yellow, error red.
// Body is left as-is (may be multi-line). The block ends in a
// horizontal rule so the actual output is visually separated.
func RenderNotice(n RemoteNotice) string {
	var color string
	switch n.Kind {
	case "success":
		color = "\033[38;5;2m" // green
	case "error":
		color = "\033[38;5;1m" // red
	default: // warning + unknown
		color = "\033[38;5;3m" // yellow
	}
	const reset = "\033[0m"
	const rule = "─────────────────────────────────────────────────────\n"
	return rule + color + n.Title + reset + "\n" + n.Body + "\n" + rule + "\n"
}

// ── SSE parsing ──────────────────────────────────────────────

func parseSSEResponse(r io.Reader) (*RemoteQueryResult, error) {
	result := &RemoteQueryResult{}
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var event map[string]any
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}

		switch event["type"] {
		case "line":
			if rowData, ok := event["data"].(map[string]any); ok {
				result.Rows = append(result.Rows, mapToResultRow(rowData))
			}
		case "stderr":
			// Render stderr in red ANSI so the shell displays it appropriately.
			if text, ok := event["text"].(string); ok {
				result.Text += "\033[38;5;1m" + text + "\033[0m"
			}
		case "notice":
			result.Notices = append(result.Notices, RemoteNotice{
				Key:   stringOf(event["key"]),
				Kind:  stringOf(event["kind"]),
				Title: stringOf(event["title"]),
				Body:  stringOf(event["body"]),
			})
		case "error":
			if msg, ok := event["message"].(string); ok {
				return nil, fmt.Errorf("%s", msg)
			}
		case "done":
			return result, nil
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// PendingSource represents a source awaiting schema configuration.
type PendingSource struct {
	App        string          `json:"app"`
	Namespace  string          `json:"namespace"`
	Instance   string          `json:"instance"`
	Deployment string          `json:"deployment"`
	Sample     json.RawMessage `json:"sample"`
	Detection  json.RawMessage `json:"detection"`
	CreatedAt  string          `json:"created_at"`
}

// ListPendingSources returns all pending sources for the authenticated customer.
func (h *HTTPCoordinator) ListPendingSources(namespace string) ([]PendingSource, error) {
	url := h.baseURL + "/api/sources/pending"
	if namespace != "" {
		url += "?namespace=" + namespace
	}
	req, _ := http.NewRequest("GET", url, nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var sources []PendingSource
	if err := json.NewDecoder(resp.Body).Decode(&sources); err != nil {
		return nil, err
	}
	return sources, nil
}

// ConfigureSource confirms the schema for a pending source.
func (h *HTTPCoordinator) ConfigureSource(namespace, app string, config map[string]any) error {
	body, _ := json.Marshal(config)
	url := fmt.Sprintf("%s/api/sources/%s/%s/configure", h.baseURL, namespace, app)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func mapToResultRow(m map[string]any) *pb.ResultRow {
	row := &pb.ResultRow{}
	if v, ok := m["raw"].(string); ok {
		row.Raw = v
	}
	return row
}

// ── Sources (CloudWatch + weftd) ───────────────────────────────

// AddCloudWatchSourceRequest mirrors the JSON body accepted by
// POST /api/sources/cloudwatch.
type AddCloudWatchSourceRequest struct {
	Namespace    string `json:"namespace"`
	App          string `json:"app"`
	AWSAccountID string `json:"aws_account_id,omitempty"`
	RoleARN      string `json:"role_arn"`
	ExternalID   string `json:"external_id,omitempty"`
	LogGroup     string `json:"log_group"`
	Region       string `json:"region"`
	PollInterval string `json:"poll_interval,omitempty"`

	// Schema fields (normally arrive via the dashboard's schema editor).
	// The CLI passes these straight through; the api server builds the
	// schema YAML server-side from them, same as the frontend path.
	Format          string `json:"format"`
	TimestampField  string `json:"timestamp_field,omitempty"`
	TimestampFormat string `json:"timestamp_format,omitempty"`
	LevelField      string `json:"level_field,omitempty"`
	LevelPattern    string `json:"level_pattern,omitempty"`
}

// AddCloudWatchSource registers a CloudWatch source.
func (h *HTTPCoordinator) AddCloudWatchSource(req AddCloudWatchSourceRequest) error {
	body, _ := json.Marshal(req)
	r, _ := http.NewRequest("POST", h.baseURL+"/api/sources/cloudwatch", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	h.setAuthHeaders(r)

	resp, err := h.client.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// ListSources returns every configured source for the caller's customer.
func (h *HTTPCoordinator) ListSources() ([]SourceInfo, error) {
	req, _ := http.NewRequest("GET", h.baseURL+"/api/sources", nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var sources []SourceInfo
	if err := json.NewDecoder(resp.Body).Decode(&sources); err != nil {
		return nil, err
	}
	return sources, nil
}

// RemoveSource deletes the source at (namespace, app). app is the
// SUB-app name (not "namespace/app"); the api rebuilds the full key.
func (h *HTTPCoordinator) RemoveSource(namespace, app string) error {
	url := fmt.Sprintf("%s/api/sources/%s/%s", h.baseURL, namespace, app)
	req, _ := http.NewRequest("DELETE", url, nil)
	h.setAuthHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
