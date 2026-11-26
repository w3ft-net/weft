//go:build !(js && wasm)

package shell

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/w3ft-net/weft/warning"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/w3ft-net/weft/chart"
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
	return b.apiListNames("/api/crons", "crons")
}

func (b *HTTPBackend) GetWatch(name string) (*WatchData, error) {
	data, err := b.apiGet("/api/crons/" + name)
	if err != nil {
		return nil, err
	}
	var w struct {
		Schedule string `json:"Schedule"`
		Pipeline string `json:"Pipeline"`
		Enabled  bool   `json:"Enabled"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("parse cron: %w", err)
	}
	return &WatchData{
		Schedule: w.Schedule,
		Pipeline: w.Pipeline,
		Enabled:  w.Enabled,
	}, nil
}

// SaveWatch's error may be a non-blocking warning.Warning (or an
// errors.Join of several) — the save already happened, but the
// backend has something to tell the user (e.g. the /cron.d
// truncation lint). store.Split separates that from an actual
// rejection. Generic: this relays whatever the "warnings" field of
// the PUT response carries, without knowing what produced it.
func (b *HTTPBackend) SaveWatch(name string, w *WatchData) error {
	body, err := b.apiPutBody("/api/crons/"+name, map[string]string{
		"schedule": w.Schedule,
		"pipeline": w.Pipeline,
	})
	if err != nil {
		return err
	}
	var resp struct {
		Warnings []string `json:"warnings"`
	}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &resp) // best-effort: a malformed/empty body just means no warnings to relay
	}
	if len(resp.Warnings) == 0 {
		return nil
	}
	warnings := make([]error, len(resp.Warnings))
	for i, m := range resp.Warnings {
		warnings[i] = warning.New("%s", m)
	}
	return errors.Join(warnings...)
}

func (b *HTTPBackend) DeleteWatch(name string) error {
	return b.apiDelete("/api/crons/" + name)
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

// GetCronSeries satisfies chart.CronSource: the cron's stored points over
// the range spec, which the server resolves.
func (b *HTTPBackend) GetCronSeries(name, rangeSpec string) (chart.CronSeries, error) {
	data, err := b.apiGet("/api/crons/" + url.PathEscape(name) + "/chart?spec=" + url.QueryEscape(rangeSpec))
	if err != nil {
		return chart.CronSeries{}, err
	}
	return DecodeCronSeries(data)
}

// GetLatestRun returns the points of a cron's latest run, in table order,
// for dashboard table and counter tiles.
func (b *HTTPBackend) GetLatestRun(name string) ([]chart.LatestPoint, error) {
	data, err := b.apiGet("/api/crons/" + url.PathEscape(name) + "/latest")
	if err != nil {
		return nil, err
	}
	var points []chart.LatestPoint
	if err := json.Unmarshal(data, &points); err != nil {
		return nil, err
	}
	return points, nil
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
	_, err := b.apiPutBody(endpoint, payload)
	return err
}

// apiPutBody is apiPut plus the response body on success, for callers
// that need to parse a JSON reply (e.g. SaveWatch's "warnings" field)
// rather than just success/failure.
func (b *HTTPBackend) apiPutBody(endpoint string, payload any) ([]byte, error) {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PUT", b.baseURL+endpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	b.setAuthHeaders(req)

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
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
			// A cron lists its path (<namespace>/<name>), the name every call takes.
			var items []struct {
				Name string
				Path string `json:"path"`
			}
			if json.Unmarshal(raw, &items) == nil {
				names := make([]string, len(items))
				for i, item := range items {
					names[i] = item.Name
					if item.Path != "" {
						names[i] = item.Path
					}
				}
				return names, nil
			}
		}
	}

	return []string{}, nil
}
