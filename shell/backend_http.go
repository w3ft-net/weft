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

	"github.com/kevlarcon/weft/chart"
)

// WatchData holds the deserialized fields of a cron watch.
type WatchData struct {
	Schedule string
	Pipeline string
	Enabled  bool
	Title    string
}

// HTTPBackend implements Backend over the weft HTTP API.
type HTTPBackend struct {
	baseURL string
	token   string
	org     string // X-Active-Org header value; empty omits the header
	client  *http.Client
}

// NewHTTPBackend creates a backend using the coordinator HTTP API.
// org is the X-Active-Org value to send on every request — empty
// lets the server fall back to the user's last-active or oldest
// membership.
func NewHTTPBackend(baseURL, token, org string) *HTTPBackend {
	return &HTTPBackend{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
		org:     org,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// setAuthHeaders applies Authorization + X-Active-Org. Centralized
// so a new endpoint can't forget the org header (same pattern as
// HTTPCoordinator).
func (b *HTTPBackend) setAuthHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+b.token)
	if b.org != "" {
		req.Header.Set("X-Active-Org", b.org)
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

func (b *HTTPBackend) GetNotifyConfig() (string, error) {
	data, err := b.apiGet("/api/notify")
	if err != nil {
		// An unset notify.yaml is a 404 from the API by design; the shell
		// should see that as an empty file, not an error.
		if err.Error() == "not found" {
			return "", nil
		}
		return "", err
	}
	var resp struct {
		AliasesYAML string `json:"aliases_yaml"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("parse notify config: %w", err)
	}
	return resp.AliasesYAML, nil
}

func (b *HTTPBackend) SaveNotifyConfig(content string) error {
	return b.apiPut("/api/notify", map[string]string{"aliases_yaml": content})
}

// ── Dashboards (/etc/dashboards/) ──────────────────────────────

func (b *HTTPBackend) ListDashboards() ([]string, error) {
	data, err := b.apiGet("/api/dashboards")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Dashboards []struct {
			Name string `json:"name"`
		} `json:"dashboards"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse dashboards: %w", err)
	}
	out := make([]string, len(resp.Dashboards))
	for i, d := range resp.Dashboards {
		out[i] = d.Name
	}
	return out, nil
}

func (b *HTTPBackend) GetDashboard(name string) (string, error) {
	data, err := b.apiGet("/api/dashboards/" + name)
	if err != nil {
		return "", err
	}
	var resp struct {
		YAMLBody string `json:"yaml_body"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("parse dashboard: %w", err)
	}
	return resp.YAMLBody, nil
}

func (b *HTTPBackend) SaveDashboard(name, body string) error {
	return b.apiPut("/api/dashboards/"+name, map[string]string{"yaml_body": body})
}

func (b *HTTPBackend) DeleteDashboard(name string) error {
	return b.apiDelete("/api/dashboards/" + name)
}

// GetWatchResults returns recent watch runs. Used by the shell to render
// sparkline / last-run values inside the interactive UI. limit is the
// server-side max; a limit of 0 tells the api to apply its default.
func (b *HTTPBackend) GetWatchResults(name string, limit int) ([]chart.WatchResultValue, error) {
	url := "/api/watches/" + name + "/results"
	if limit > 0 {
		url += "?limit=" + strconv.Itoa(limit)
	}
	data, err := b.apiGet(url)
	if err != nil {
		return nil, err
	}
	// Tags must match the API's GORM-default JSON shape exactly —
	// `RanAt` (capital R, no underscore). A `json:"ran_at"` tag fails
	// case-insensitive matching because of the underscore, leaving
	// RanAt empty and chart_builtin's RFC3339 parse silently rejecting
	// every row. (See api/handlers.go:1524: handleWatchResults dumps
	// the model directly with json.NewEncoder.)
	var rows []struct {
		Stdout string `json:"Stdout"`
		RanAt  string `json:"RanAt"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}
	out := make([]chart.WatchResultValue, 0, len(rows))
	for _, r := range rows {
		out = append(out, chart.WatchResultValue{
			Value: strings.TrimSpace(r.Stdout),
			RanAt: r.RanAt,
		})
	}
	return out, nil
}

// GetFile reads a file via the generic /api/fs/read endpoint.
func (b *HTTPBackend) GetFile(path string) ([]byte, error) {
	return b.apiGet("/api/fs/read?path=" + urlQueryEscape(path))
}

// urlQueryEscape inlines a tiny escape so we don't drag net/url just
// for one query-parameter encoding.
func urlQueryEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'),
			c == '-' || c == '_' || c == '.' || c == '~' || c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// ── HTTP helpers ──────────────────────────────────────────────

func (b *HTTPBackend) apiGet(endpoint string) ([]byte, error) {
	req, _ := http.NewRequest("GET", b.baseURL+endpoint, nil)
	b.setAuthHeaders(req)

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
	b.setAuthHeaders(req)

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
	b.setAuthHeaders(req)

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
