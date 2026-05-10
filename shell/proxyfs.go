package shell

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/weft/chart"
	"github.com/kevlarcon/weft/store"
)

// ProxyFS wraps a MountFS (backed by ProxyMounts) and adds the extra
// methods needed by chart.WatchResultsProvider and dashboard.Provider.
// It is the sole CLI-side FS implementation.
type ProxyFS struct {
	*shellcore.MountFS

	baseURL string
	token   string
	org     string
	client  *http.Client

	mu        sync.RWMutex
	specCache map[string]*store.DashboardSpec
}

// NewProxyFS constructs a ProxyFS that routes all FS operations through
// the server's /api/fs/ endpoints and provides dashboard/chart data.
func NewProxyFS(baseURL, token, org string) *ProxyFS {
	return &ProxyFS{
		MountFS:   NewProxyMountFS(baseURL, token, org),
		baseURL:   baseURL,
		token:     token,
		org:       org,
		client:    &http.Client{Timeout: 30 * time.Second},
		specCache: make(map[string]*store.DashboardSpec),
	}
}

func (p *ProxyFS) setAuth(req *http.Request) {
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	if p.org != "" {
		req.Header.Set("X-Active-Org", p.org)
	}
}

// GetWatchResults satisfies chart.WatchResultsProvider.
func (p *ProxyFS) GetWatchResults(name string, limit int) ([]chart.WatchResultValue, error) {
	u := fmt.Sprintf("%s/api/watches/%s/results", p.baseURL, url.PathEscape(name))
	req, _ := http.NewRequest("GET", u, nil)
	p.setAuth(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(body)))
	}

	var items []struct {
		Stdout string `json:"Stdout"`
		RanAt  string `json:"RanAt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}

	n := limit
	if n <= 0 {
		n = 20
	}
	if n > len(items) {
		n = len(items)
	}

	results := make([]chart.WatchResultValue, n)
	for i := 0; i < n; i++ {
		results[i] = chart.WatchResultValue{
			Value: items[i].Stdout,
			RanAt: items[i].RanAt,
		}
	}
	return results, nil
}

// GetDashboard satisfies dashboard.Provider.
func (p *ProxyFS) GetDashboard(name string) (string, error) {
	data, err := p.MountFS.ReadFile("/etc/dashboards/" + name + ".yaml")
	if err != nil {
		return "", err
	}
	body := string(data)
	if spec, perr := store.ParseDashboardYAML(body); perr == nil {
		p.mu.Lock()
		p.specCache[name] = spec
		p.mu.Unlock()
	}
	return body, nil
}

// GetDashboardSpec satisfies dashboard.Provider.
func (p *ProxyFS) GetDashboardSpec(name string) (*store.DashboardSpec, error) {
	p.mu.RLock()
	spec, ok := p.specCache[name]
	p.mu.RUnlock()
	if ok {
		return spec, nil
	}
	body, err := p.GetDashboard(name)
	if err != nil {
		return nil, err
	}
	return store.ParseDashboardYAML(body)
}

// ListDashboards satisfies dashboard.Provider.
func (p *ProxyFS) ListDashboards() ([]string, error) {
	entries, err := p.MountFS.ReadDir("/etc/dashboards")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		n := e
		n = strings.TrimSuffix(n, ".yaml")
		names = append(names, n)
	}
	return names, nil
}

// Prefetch pre-warms the server-side caches by calling ReadDir on
// key paths. This triggers the server to fetch data that will be
// needed for tab completion.
func (p *ProxyFS) Prefetch() {
	var wg sync.WaitGroup
	paths := []string{"/bin", "/cron.d", "/etc/dashboards"}
	wg.Add(len(paths))
	for _, path := range paths {
		go func(path string) {
			defer wg.Done()
			p.MountFS.ReadDir(path)
		}(path)
	}
	wg.Wait()
}
