//go:build !(js && wasm)

package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
	"github.com/graybeardcorp/warp/pkg/shell/mounts"
)

// proxyBackend is the transport behind the CLI's mounts: every operation
// is a request to the server's /api/fs/ endpoints. warp's mounts.Remote
// turns it into a shellcore.Mount, one per virtual directory (/bin,
// /cron.d, /etc, /log, /tmp). Paths it receives are absolute ("/bin/scan"),
// which is what the API takes, so one backend serves every mount.
type proxyBackend struct {
	client  *http.Client
	baseURL string
	token   string
	org     string
}

var (
	_ mounts.Backend     = (*proxyBackend)(nil)
	_ mounts.StatBackend = (*proxyBackend)(nil)
	_ mounts.WalkBackend = (*proxyBackend)(nil)
)

func (p *proxyBackend) setAuth(req *http.Request) {
	if p.token != "" {
		req.Header.Set("Authorization", Authorization(p.token))
	}
	if p.org != "" {
		req.Header.Set("X-Active-Org", p.org)
	}
}

// do sends a request built with the run's context, so Ctrl+C cancels it,
// and turns an error status into an error carrying the server's message.
// The caller closes the body.
func (p *proxyBackend) do(ctx context.Context, method, endpoint string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+endpoint, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	p.setAuth(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

func (p *proxyBackend) List(ctx context.Context, path string) ([]mounts.Entry, error) {
	resp, err := p.do(ctx, "GET", "/api/fs/list?path="+url.QueryEscape(path), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Entries []struct {
			Name  string `json:"name"`
			IsDir bool   `json:"is_dir"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	entries := make([]mounts.Entry, len(data.Entries))
	for i, e := range data.Entries {
		entries[i] = mounts.Entry{Name: e.Name, Dir: e.IsDir}
	}
	return entries, nil
}

// Walk returns the entire subtree in one round trip, as paths relative to
// path, so `tree` makes one request instead of one per directory (301 for
// a 7,500-app /log tree). The server resolves the walk in-process — for
// /log via LogMount's single-pass, memoised-permission fast path.
func (p *proxyBackend) Walk(ctx context.Context, path string) ([]string, error) {
	resp, err := p.do(ctx, "GET", "/api/fs/list?path="+url.QueryEscape(path)+"&walk=1", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data.Paths, nil
}

func (p *proxyBackend) Read(ctx context.Context, path string) ([]byte, error) {
	resp, err := p.do(ctx, "GET", "/api/fs/read?path="+url.QueryEscape(path), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Stat has no endpoint of its own. A path that reads is a file of that
// size; one that doesn't read but lists is a directory.
func (p *proxyBackend) Stat(ctx context.Context, path string) (mounts.Entry, error) {
	if data, err := p.Read(ctx, path); err == nil {
		return mounts.Entry{Size: int64(len(data))}, nil
	}
	if _, err := p.List(ctx, path); err == nil {
		return mounts.Entry{Dir: true}, nil
	}
	return mounts.Entry{}, fs.ErrNotExist
}

func (p *proxyBackend) Write(ctx context.Context, path string, data []byte) error {
	body, _ := json.Marshal(map[string]string{"path": path, "content": string(data)})
	resp, err := p.do(ctx, "PUT", "/api/fs/write", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (p *proxyBackend) Remove(ctx context.Context, path string) error {
	resp, err := p.do(ctx, "DELETE", "/api/fs/remove?path="+url.QueryEscape(path), nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// proxyMounts are the weft virtual directories and whether each is
// writable. /log is the read-only query namespace; the rest are writable
// local trees. (Mounts no longer carry a "kind" — the server owns
// query-vs-local routing now; writability is the only capability the
// client still tracks, for the FS write/remove gates.)
var proxyMounts = []struct {
	prefix   string
	writable bool
}{
	{"/bin", true},
	{"/cron.d", true},
	{"/etc", true},
	{"/log", false},
	{"/tmp", true},
}

// newProxyMount mounts the server's tree at prefix through b.
func newProxyMount(prefix string, writable bool, b *proxyBackend) shellcore.Mount {
	var opts []mounts.Option
	if !writable {
		opts = append(opts, mounts.ReadOnly())
	}
	return mounts.Remote(prefix, b, opts...)
}

// NewProxyMountFS constructs a MountFS with proxy mounts for all weft
// virtual directories, pointing at the given API endpoint.
func NewProxyMountFS(baseURL, token, org string) *shellcore.MountFS {
	b := &proxyBackend{
		client:  &http.Client{Timeout: 30 * time.Second},
		baseURL: baseURL,
		token:   token,
		org:     org,
	}
	mfs := shellcore.NewMountFS()
	for _, d := range proxyMounts {
		mfs.Register(newProxyMount(d.prefix, d.writable, b))
	}
	return mfs
}
