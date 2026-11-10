//go:build !(js && wasm)

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

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/chart"
	"github.com/graybeardcorp/weft/store"
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

// GetCronSeries satisfies chart.CronSource.
func (p *ProxyFS) GetCronSeries(name, rangeSpec string) (chart.CronSeries, error) {
	u := fmt.Sprintf("%s/api/crons/%s/chart?spec=%s", p.baseURL, url.PathEscape(name), url.QueryEscape(rangeSpec))
	req, _ := http.NewRequest("GET", u, nil)
	p.setAuth(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return chart.CronSeries{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return chart.CronSeries{}, err
	}
	if resp.StatusCode >= 400 {
		return chart.CronSeries{}, fmt.Errorf("%s", strings.TrimSpace(string(body)))
	}
	return DecodeCronSeries(body)
}

// GetLatestRun returns the points of a cron's latest run, in table order,
// for dashboard table and counter tiles.
func (p *ProxyFS) GetLatestRun(name string) ([]chart.LatestPoint, error) {
	u := fmt.Sprintf("%s/api/crons/%s/latest", p.baseURL, url.PathEscape(name))
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
	var points []chart.LatestPoint
	if err := json.NewDecoder(resp.Body).Decode(&points); err != nil {
		return nil, err
	}
	return points, nil
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
		names = append(names, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	return names, nil
}

// Prefetch pre-warms the server-side caches by calling ReadDir on
// key paths. This triggers the server to fetch data that will be
// needed for tab completion. Crons live one level down, in a directory per
// namespace, so /cron.d is warmed with one walk request that covers every
// namespace directory instead of a listing for each.
func (p *ProxyFS) Prefetch() {
	var wg sync.WaitGroup
	paths := []string{"/bin", "/etc/dashboards"}
	wg.Add(len(paths) + 1)
	for _, path := range paths {
		go func(path string) {
			defer wg.Done()
			p.MountFS.ReadDir(path)
		}(path)
	}
	go func() {
		defer wg.Done()
		cron := &ProxyMount{prefix: "/cron.d", client: p.client, baseURL: p.baseURL, token: p.token, org: p.org}
		cron.Walk("")
	}()
	wg.Wait()
}
