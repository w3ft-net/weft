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
	Rows []*pb.ResultRow
	Text string
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
	client       *http.Client
	streamClient *http.Client
}

func NewHTTPCoordinator(baseURL, token string) *HTTPCoordinator {
	return &HTTPCoordinator{
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		token:        token,
		client:       &http.Client{Timeout: 10 * time.Second},
		streamClient: &http.Client{},
	}
}

func (h *HTTPCoordinator) Close() error { return nil }

func (h *HTTPCoordinator) Query(pipeline string, start, end time.Time) (*RemoteQueryResult, error) {
	body, _ := json.Marshal(map[string]any{
		"pipeline": pipeline,
		"follow":   false,
	})

	req, err := http.NewRequest("POST", h.baseURL+"/api/shell/exec", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("unauthorized — run `weft login` or check WEFT_TOKEN")
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	return parseSSEResponse(resp.Body)
}

func (h *HTTPCoordinator) Follow(ctx context.Context, pipeline string, onRow func(*pb.ResultRow)) error {
	body, _ := json.Marshal(map[string]any{
		"pipeline": pipeline,
		"follow":   true,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", h.baseURL+"/api/shell/exec", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)

	// streamClient has no wall-clock Timeout — the request lives for the
	// duration of the caller's ctx.
	resp, err := h.streamClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

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
		case "row":
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
	req.Header.Set("Authorization", "Bearer "+h.token)

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
	req.Header.Set("Authorization", "Bearer "+h.token)

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
	req.Header.Set("Authorization", "Bearer "+h.token)

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
	req.Header.Set("Authorization", "Bearer "+h.token)

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
	req.Header.Set("Authorization", "Bearer "+h.token)

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
	req.Header.Set("Authorization", "Bearer "+h.token)

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
		case "output":
			if text, ok := event["text"].(string); ok {
				result.Text += text
			}
		case "row":
			if rowData, ok := event["data"].(map[string]any); ok {
				result.Rows = append(result.Rows, mapToResultRow(rowData))
			}
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
	req.Header.Set("Authorization", "Bearer "+h.token)

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
	req.Header.Set("Authorization", "Bearer "+h.token)

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
	if v, ok := m["line_num"].(float64); ok {
		row.LineNum = int64(v)
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
	r.Header.Set("Authorization", "Bearer "+h.token)

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
	req.Header.Set("Authorization", "Bearer "+h.token)

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
	req.Header.Set("Authorization", "Bearer "+h.token)

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
