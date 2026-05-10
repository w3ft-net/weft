package shell

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kevlarcon/warp/pkg/shellcore"
)

// ProxyMount implements shellcore.Mount by proxying all operations
// through the server's /api/fs/ endpoints. One ProxyMount is
// registered per virtual directory (/bin, /cron.d, /etc, /log, /tmp).
type ProxyMount struct {
	prefix  string
	caps    shellcore.MountCaps
	client  *http.Client
	baseURL string
	token   string
	org     string
}

func (p *ProxyMount) Prefix() string            { return p.prefix }
func (p *ProxyMount) Caps() shellcore.MountCaps  { return p.caps }

func (p *ProxyMount) fullPath(rel string) string {
	if rel == "" {
		return p.prefix
	}
	return p.prefix + "/" + rel
}

func (p *ProxyMount) setAuth(req *http.Request) {
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	if p.org != "" {
		req.Header.Set("X-Active-Org", p.org)
	}
}

func (p *ProxyMount) ReadDir(rel string) ([]fs.DirEntry, error) {
	path := p.fullPath(rel)
	u := fmt.Sprintf("%s/api/fs/list?path=%s", p.baseURL, url.QueryEscape(path))
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

	var data struct {
		Entries []struct {
			Name  string `json:"name"`
			IsDir bool   `json:"is_dir"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	entries := make([]fs.DirEntry, len(data.Entries))
	for i, e := range data.Entries {
		entries[i] = &proxyDirEntry{name: e.Name, dir: e.IsDir}
	}
	return entries, nil
}

func (p *ProxyMount) ReadFile(rel string) ([]byte, error) {
	path := p.fullPath(rel)
	u := fmt.Sprintf("%s/api/fs/read?path=%s", p.baseURL, url.QueryEscape(path))
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
	return io.ReadAll(resp.Body)
}

func (p *ProxyMount) Stat(rel string) (fs.FileInfo, error) {
	// Stat isn't exposed as a separate endpoint yet — infer from ReadDir
	// or return synthetic info. For now, return a minimal stat that
	// satisfies ls -l.
	if rel == "" {
		return &proxyFileInfo{name: strings.TrimPrefix(p.prefix, "/"), dir: true}, nil
	}
	// Try ReadFile to check existence + get size.
	data, err := p.ReadFile(rel)
	if err != nil {
		// Could be a directory — try ReadDir.
		if _, dirErr := p.ReadDir(rel); dirErr == nil {
			return &proxyFileInfo{name: lastSeg(rel), dir: true}, nil
		}
		return nil, fs.ErrNotExist
	}
	return &proxyFileInfo{name: lastSeg(rel), size: int64(len(data))}, nil
}

func (p *ProxyMount) WriteFile(rel string, data []byte) error {
	path := p.fullPath(rel)
	body, _ := json.Marshal(map[string]string{"path": path, "content": string(data)})
	u := fmt.Sprintf("%s/api/fs/write", p.baseURL)
	req, _ := http.NewRequest("PUT", u, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	p.setAuth(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	return nil
}

func (p *ProxyMount) Remove(rel string) error {
	path := p.fullPath(rel)
	u := fmt.Sprintf("%s/api/fs/remove?path=%s", p.baseURL, url.QueryEscape(path))
	req, _ := http.NewRequest("DELETE", u, nil)
	p.setAuth(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	return nil
}

// NewProxyMountFS constructs a MountFS with proxy mounts for all weft
// virtual directories, pointing at the given API endpoint.
func NewProxyMountFS(baseURL, token, org string) *shellcore.MountFS {
	client := &http.Client{Timeout: 30 * time.Second}
	mfs := shellcore.NewMountFS()

	type mountDef struct {
		prefix   string
		kind     shellcore.MountKind
		writable bool
	}
	defs := []mountDef{
		{"/bin", shellcore.MountLocal, true},
		{"/cron.d", shellcore.MountLocal, true},
		{"/etc", shellcore.MountLocal, true},
		{"/log", shellcore.MountQuery, false},
		{"/tmp", shellcore.MountLocal, true},
	}
	for _, d := range defs {
		mfs.Register(&ProxyMount{
			prefix:  d.prefix,
			caps:    shellcore.MountCaps{Kind: d.kind, Writable: d.writable},
			client:  client,
			baseURL: baseURL,
			token:   token,
			org:     org,
		})
	}
	return mfs
}

func lastSeg(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// --- fs types ---

type proxyFileInfo struct {
	name  string
	dir   bool
	size  int64
	mtime time.Time
}

func (fi *proxyFileInfo) Name() string      { return fi.name }
func (fi *proxyFileInfo) Size() int64       { return fi.size }
func (fi *proxyFileInfo) Mode() fs.FileMode { if fi.dir { return fs.ModeDir | 0755 }; return 0644 }
func (fi *proxyFileInfo) ModTime() time.Time { return fi.mtime }
func (fi *proxyFileInfo) IsDir() bool       { return fi.dir }
func (fi *proxyFileInfo) Sys() any          { return nil }

type proxyDirEntry struct {
	name string
	dir  bool
}

func (de *proxyDirEntry) Name() string              { return de.name }
func (de *proxyDirEntry) IsDir() bool               { return de.dir }
func (de *proxyDirEntry) Type() fs.FileMode         { if de.dir { return fs.ModeDir }; return 0 }
func (de *proxyDirEntry) Info() (fs.FileInfo, error) { return &proxyFileInfo{name: de.name, dir: de.dir}, nil }
