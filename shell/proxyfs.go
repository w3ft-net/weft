//go:build !(js && wasm)

package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
	"github.com/w3ft-net/weft/chart"
	"github.com/w3ft-net/weft/dashboard"
)

// ProxyFS wraps a MountFS (backed by proxyBackend mounts) and adds the extra
// methods needed by chart.WatchResultsProvider and dashboard.Provider.
// It is the sole CLI-side FS implementation.
type ProxyFS struct {
	*shellcore.MountFS

	baseURL    string
	token      string
	decorators []RequestDecorator
	client     *http.Client

	mu        sync.RWMutex
	specCache map[string]*dashboard.Spec
}

// NewProxyFS constructs a ProxyFS that routes all FS operations through
// the server's /api/fs/ endpoints and provides dashboard/chart data.
func NewProxyFS(baseURL, token string, decorators ...RequestDecorator) *ProxyFS {
	return &ProxyFS{
		MountFS:    NewProxyMountFS(baseURL, token, decorators...),
		baseURL:    baseURL,
		token:      token,
		decorators: decorators,
		client:     &http.Client{Timeout: 30 * time.Second},
		specCache:  make(map[string]*dashboard.Spec),
	}
}

func (p *ProxyFS) setAuth(req *http.Request) {
	if p.token != "" {
		req.Header.Set("Authorization", Authorization(p.token))
	}
	decorate(req, p.decorators)
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
	data, err := p.MountFS.ReadFile(context.Background(), "/etc/dashboards/"+name+".yaml")
	if err != nil {
		return "", err
	}
	body := string(data)
	if spec, perr := dashboard.Parse(body); perr == nil {
		p.mu.Lock()
		p.specCache[name] = spec
		p.mu.Unlock()
	}
	return body, nil
}

// GetDashboardSpec satisfies dashboard.Provider.
func (p *ProxyFS) GetDashboardSpec(name string) (*dashboard.Spec, error) {
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
	return dashboard.Parse(body)
}

// ListDashboards satisfies dashboard.Provider.
func (p *ProxyFS) ListDashboards() ([]string, error) {
	entries, err := p.MountFS.ReadDir(context.Background(), "/etc/dashboards")
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
			p.MountFS.ReadDir(context.Background(), path)
		}(path)
	}
	go func() {
		defer wg.Done()
		b := &proxyBackend{client: p.client, baseURL: p.baseURL, token: p.token, decorators: p.decorators}
		b.Walk(context.Background(), "/cron.d")
	}()
	wg.Wait()
}
