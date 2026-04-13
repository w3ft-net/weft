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

// HTTPCoordinator talks to the weft API over HTTP instead of gRPC.
// This works through the ALB / public internet.
type HTTPCoordinator struct {
	baseURL string // e.g., "https://api.w3ft.net"
	token   string // JWT for Authorization header
	client  *http.Client
}

func NewHTTPCoordinator(baseURL, token string) *HTTPCoordinator {
	return &HTTPCoordinator{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 30 * time.Second},
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

func (h *HTTPCoordinator) Follow(ctx context.Context, pipeline string, onRow func(*pb.LogRow)) error {
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

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	scanner := bufio.NewScanner(resp.Body)
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
				onRow(mapToLogRow(rowData))
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

func (h *HTTPCoordinator) CreateToken(customer, label, app string) (string, error) {
	body, _ := json.Marshal(map[string]string{"label": label, "app": app})
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

func (h *HTTPCoordinator) RevokeToken(token string) error {
	req, _ := http.NewRequest("DELETE", h.baseURL+"/api/tokens/"+token, nil)
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
				result.Rows = append(result.Rows, mapToLogRow(rowData))
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

func mapToLogRow(m map[string]any) *pb.LogRow {
	row := &pb.LogRow{}
	if v, ok := m["raw"].(string); ok {
		row.Raw = v
	}
	if v, ok := m["app"].(string); ok {
		row.App = v
	}
	if v, ok := m["level"].(string); ok {
		row.Level = v
	}
	if v, ok := m["instance"].(string); ok {
		row.Instance = v
	}
	if v, ok := m["ts_unix_ns"].(float64); ok {
		row.TsUnixNs = int64(v)
	}
	if v, ok := m["line_num"].(float64); ok {
		row.LineNum = int64(v)
	}
	if payload, ok := m["payload"]; ok {
		row.PayloadJson, _ = json.Marshal(payload)
	}
	return row
}
