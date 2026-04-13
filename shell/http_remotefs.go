package shell

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPRemoteFS implements shellcore.FS by calling the weft HTTP API.
type HTTPRemoteFS struct {
	baseURL string
	token   string
	apps    []string
	client  *http.Client
}

func NewHTTPRemoteFS(baseURL, token string, apps []string) *HTTPRemoteFS {
	return &HTTPRemoteFS{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
		apps:    apps,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (f *HTTPRemoteFS) ReadFile(path string) ([]byte, error) {
	dir, name := splitFSPath(normFSPath(path))

	switch dir {
	case "bin":
		return f.apiGet("/api/functions/" + name)
	case "cron.d":
		data, err := f.apiGet("/api/watches/" + name)
		if err != nil {
			return nil, err
		}
		var w struct {
			Schedule  string `json:"Schedule"`
			Pipeline  string `json:"Pipeline"`
			NotifyDest string `json:"NotifyDest"`
			Enabled   bool   `json:"Enabled"`
		}
		if json.Unmarshal(data, &w) == nil {
			var b strings.Builder
			fmt.Fprintf(&b, "# schedule: %s\n", w.Schedule)
			if w.NotifyDest != "" {
				fmt.Fprintf(&b, "# notify: %s\n", w.NotifyDest)
			}
			if !w.Enabled {
				b.WriteString("# enabled: false\n")
			}
			b.WriteString(w.Pipeline)
			if !strings.HasSuffix(w.Pipeline, "\n") {
				b.WriteByte('\n')
			}
			return []byte(b.String()), nil
		}
		return data, nil
	}

	return nil, fmt.Errorf("%s: not found", path)
}

func (f *HTTPRemoteFS) WriteFile(path string, data []byte) error {
	dir, name := splitFSPath(normFSPath(path))

	switch dir {
	case "bin":
		return f.apiPut("/api/functions/"+name, map[string]string{"source": string(data)})
	case "cron.d":
		schedule, pipeline, notifyDest := parseCronFileContent(string(data))
		return f.apiPut("/api/watches/"+name, map[string]string{
			"schedule":    schedule,
			"pipeline":    pipeline,
			"notify_dest": notifyDest,
		})
	}

	return fmt.Errorf("%s: read-only", path)
}

func (f *HTTPRemoteFS) ReadDir(path string) ([]string, error) {
	p := normFSPath(path)

	switch p {
	case "/":
		return []string{"bin/", "cron.d/", "log/", "etc/"}, nil
	case "/log":
		entries := make([]string, len(f.apps))
		for i, app := range f.apps {
			entries[i] = app + ".log"
		}
		return entries, nil
	case "/bin":
		return f.apiListNames("/api/functions", "functions")
	case "/cron.d":
		return f.apiListNames("/api/watches", "watches")
	case "/etc":
		return []string{"usage"}, nil
	}

	return nil, fmt.Errorf("%s: no such directory", path)
}

func (f *HTTPRemoteFS) Remove(path string) error {
	dir, name := splitFSPath(normFSPath(path))

	switch dir {
	case "bin":
		return f.apiDelete("/api/functions/" + name)
	case "cron.d":
		return f.apiDelete("/api/watches/" + name)
	}

	return fmt.Errorf("%s: operation not permitted", path)
}

// ── HTTP helpers ──────────────────────────────────────────────

func (f *HTTPRemoteFS) apiGet(endpoint string) ([]byte, error) {
	req, _ := http.NewRequest("GET", f.baseURL+endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)

	resp, err := f.client.Do(req)
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

func (f *HTTPRemoteFS) apiPut(endpoint string, payload any) error {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PUT", f.baseURL+endpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)

	resp, err := f.client.Do(req)
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

func (f *HTTPRemoteFS) apiDelete(endpoint string) error {
	req, _ := http.NewRequest("DELETE", f.baseURL+endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)

	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (f *HTTPRemoteFS) apiListNames(endpoint, key string) ([]string, error) {
	data, err := f.apiGet(endpoint)
	if err != nil {
		return []string{}, nil
	}

	// Envelope: {"functions": [...]} or {"watches": [...]}
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
