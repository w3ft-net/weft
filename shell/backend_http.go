package shell

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kevlarcon/weft/shellcore"
)

// HTTPBackend implements Backend over the weft HTTP API.
type HTTPBackend struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewHTTPBackend creates a backend using the coordinator HTTP API.
func NewHTTPBackend(baseURL, token string) *HTTPBackend {
	return &HTTPBackend{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (b *HTTPBackend) ListFunctions() ([]string, error) {
	return b.apiListNames("/api/functions", "functions")
}

func (b *HTTPBackend) GetFunction(name string) (string, error) {
	data, err := b.apiGet("/api/functions/" + name)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (b *HTTPBackend) SaveFunction(name, source string) error {
	return b.apiPut("/api/functions/"+name, map[string]string{"source": source})
}

func (b *HTTPBackend) DeleteFunction(name string) error {
	return b.apiDelete("/api/functions/" + name)
}

func (b *HTTPBackend) ListWatches() ([]string, error) {
	return b.apiListNames("/api/watches", "watches")
}

func (b *HTTPBackend) GetWatch(name string) (*WatchData, error) {
	data, err := b.apiGet("/api/watches/" + name)
	if err != nil {
		return nil, err
	}
	var w struct {
		Schedule string `json:"Schedule"`
		Pipeline string `json:"Pipeline"`
		Enabled  bool   `json:"Enabled"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("parse watch: %w", err)
	}
	return &WatchData{
		Schedule: w.Schedule,
		Pipeline: w.Pipeline,
		Enabled:  w.Enabled,
	}, nil
}

func (b *HTTPBackend) SaveWatch(name string, w *WatchData) error {
	return b.apiPut("/api/watches/"+name, map[string]string{
		"schedule": w.Schedule,
		"pipeline": w.Pipeline,
	})
}

func (b *HTTPBackend) DeleteWatch(name string) error {
	return b.apiDelete("/api/watches/" + name)
}

// GetWatchResults returns recent watch runs. Used by the shell to render
// sparkline / last-run values inside the interactive UI. limit is the
// server-side max; a limit of 0 tells the api to apply its default.
func (b *HTTPBackend) GetWatchResults(name string, limit int) ([]shellcore.WatchResultValue, error) {
	url := "/api/watches/" + name + "/results"
	if limit > 0 {
		url += "?limit=" + strconv.Itoa(limit)
	}
	data, err := b.apiGet(url)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Stdout string `json:"stdout"`
		RanAt  string `json:"ran_at"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}
	out := make([]shellcore.WatchResultValue, 0, len(rows))
	for _, r := range rows {
		out = append(out, shellcore.WatchResultValue{
			Value: strings.TrimSpace(r.Stdout),
			RanAt: r.RanAt,
		})
	}
	return out, nil
}

// ── HTTP helpers ──────────────────────────────────────────────

func (b *HTTPBackend) apiGet(endpoint string) ([]byte, error) {
	req, _ := http.NewRequest("GET", b.baseURL+endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+b.token)

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("not found")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func (b *HTTPBackend) apiPut(endpoint string, payload any) error {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PUT", b.baseURL+endpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.token)

	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", strings.TrimSpace(string(msg)))
	}
	return nil
}

func (b *HTTPBackend) apiDelete(endpoint string) error {
	req, _ := http.NewRequest("DELETE", b.baseURL+endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+b.token)

	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (b *HTTPBackend) apiListNames(endpoint, key string) ([]string, error) {
	data, err := b.apiGet(endpoint)
	if err != nil {
		return []string{}, nil
	}

	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) == nil {
		if raw, ok := envelope[key]; ok {
			var items []struct{ Name string }
			if json.Unmarshal(raw, &items) == nil {
				names := make([]string, len(items))
				for i, item := range items {
					names[i] = item.Name
				}
				return names, nil
			}
		}
	}

	return []string{}, nil
}
