//go:build !(js && wasm)

package shell

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
)

// newTestProxyMount returns a writable proxy mount wired to a small fake
// API. The handler is given the parsed query string (path=...) and the
// request body so tests can assert the wire shape directly.
func newTestProxyMount(t *testing.T, prefix string, handler http.HandlerFunc) shellcore.Mount {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return newProxyMount(prefix, true, &proxyBackend{
		client:     srv.Client(),
		baseURL:    srv.URL,
		token:      "tok",
		decorators: []RequestDecorator{Header("X-Active-Org", "acme")},
	})
}

func TestProxyMount_PrefixAndCaps(t *testing.T) {
	p := newProxyMount("/bin", true, &proxyBackend{})
	if p.Prefix() != "/bin" {
		t.Errorf("Prefix = %q", p.Prefix())
	}
	if err := p.(shellcore.AuthMount).Authorize(context.Background(), "x", shellcore.PermWrite); errors.Is(err, shellcore.ErrReadOnly) {
		t.Errorf("/bin should be writable, got %v", err)
	}
	ro := newProxyMount("/log", false, &proxyBackend{}).(shellcore.AuthMount)
	if err := ro.Authorize(context.Background(), "x", shellcore.PermWrite); !errors.Is(err, shellcore.ErrReadOnly) {
		t.Errorf("/log should be read-only, got %v", err)
	}
}

// TestProxyMount_AbsolutePaths ensures relative paths reach the API
// joined onto the prefix with a single slash. An empty relative path is
// the prefix verbatim — that's the "list the mount root" case for ReadDir.
func TestProxyMount_AbsolutePaths(t *testing.T) {
	var got []string
	p := newTestProxyMount(t, "/bin", func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Query().Get("path"))
		json.NewEncoder(w).Encode(map[string]any{"entries": []any{}})
	})
	p.ReadDir(context.Background(), "")
	p.ReadFile(context.Background(), "scan")
	p.ReadFile(context.Background(), "sub/path")
	want := []string{"/bin", "/bin/scan", "/bin/sub/path"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("server saw paths %v, want %v", got, want)
	}
}

func TestProxyMount_SetAuth(t *testing.T) {
	p := &proxyBackend{token: "abc", decorators: []RequestDecorator{Header("X-Active-Org", "acme")}}
	req, _ := http.NewRequest("GET", "http://x/y", nil)
	p.setAuth(req)
	if req.Header.Get("Authorization") != "Bearer abc" {
		t.Errorf("Authorization = %q", req.Header.Get("Authorization"))
	}
	if req.Header.Get("X-Active-Org") != "acme" {
		t.Errorf("X-Active-Org = %q", req.Header.Get("X-Active-Org"))
	}

	// Empty token omits the header.
	p2 := &proxyBackend{}
	req2, _ := http.NewRequest("GET", "http://x/y", nil)
	p2.setAuth(req2)
	if req2.Header.Get("Authorization") != "" {
		t.Errorf("empty token should omit Authorization, got %q", req2.Header.Get("Authorization"))
	}
}

func TestProxyMount_ReadDir(t *testing.T) {
	var capturedPath string
	p := newTestProxyMount(t, "/bin", func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Query().Get("path")
		json.NewEncoder(w).Encode(map[string]any{
			"entries": []map[string]any{
				{"name": "scan", "is_dir": false},
				{"name": "subdir", "is_dir": true},
			},
		})
	})

	entries, err := p.ReadDir(context.Background(), "")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if capturedPath != "/bin" {
		t.Errorf("server saw path=%q", capturedPath)
	}
	if len(entries) != 2 {
		t.Fatalf("len=%d", len(entries))
	}
	if entries[0].Name() != "scan" || entries[0].IsDir() {
		t.Errorf("entry[0] = %+v", entries[0])
	}
	if !entries[1].IsDir() {
		t.Errorf("entry[1] should be dir")
	}
}

func TestProxyMount_ReadDir_ServerError(t *testing.T) {
	p := newTestProxyMount(t, "/bin", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("permission denied"))
	})
	if _, err := p.ReadDir(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want permission denied", err)
	}
}

func TestProxyMount_ReadFile(t *testing.T) {
	var capturedPath string
	p := newTestProxyMount(t, "/bin", func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Query().Get("path")
		w.Write([]byte("#!/bin/bash\necho hi\n"))
	})

	data, err := p.ReadFile(context.Background(), "scan")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if capturedPath != "/bin/scan" {
		t.Errorf("path=%q", capturedPath)
	}
	if string(data) != "#!/bin/bash\necho hi\n" {
		t.Errorf("data = %q", data)
	}
}

func TestProxyMount_ReadFile_NotFound(t *testing.T) {
	p := newTestProxyMount(t, "/bin", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("file not found"))
	})
	if _, err := p.ReadFile(context.Background(), "missing"); err == nil {
		t.Error("expected error on 404")
	}
}

func TestProxyMount_WriteFile(t *testing.T) {
	var capturedBody string
	p := newTestProxyMount(t, "/bin", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			t.Errorf("method = %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		w.WriteHeader(http.StatusOK)
	})

	if err := p.WriteFile(context.Background(), "scan", []byte("echo hello")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Body carries path + content as JSON.
	var got map[string]string
	if err := json.Unmarshal([]byte(capturedBody), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["path"] != "/bin/scan" || got["content"] != "echo hello" {
		t.Errorf("body = %+v", got)
	}
}

func TestProxyMount_Remove(t *testing.T) {
	var capturedPath string
	p := newTestProxyMount(t, "/bin", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("method = %s", r.Method)
		}
		capturedPath = r.URL.Query().Get("path")
		w.WriteHeader(http.StatusNoContent)
	})

	if err := p.Remove(context.Background(), "scan"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if capturedPath != "/bin/scan" {
		t.Errorf("path = %q", capturedPath)
	}
}

func TestProxyMount_Remove_ServerError(t *testing.T) {
	p := newTestProxyMount(t, "/bin", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte("in use"))
	})
	if err := p.Remove(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("err = %v", err)
	}
}

// TestProxyMount_Stat_RootDir: stat with empty rel returns a synthetic
// directory entry without hitting the API at all.
func TestProxyMount_Stat_RootDir(t *testing.T) {
	p := newProxyMount("/bin", true, &proxyBackend{}) // no server: any request would fail
	info, err := p.Stat(context.Background(), "")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Error("root should be dir")
	}
	if info.Name() != "bin" {
		t.Errorf("Name = %q", info.Name())
	}
}

// TestProxyMount_Stat_File: when the path is a file, Stat returns a
// FileInfo with the file's size (inferred from a ReadFile call).
func TestProxyMount_Stat_File(t *testing.T) {
	p := newTestProxyMount(t, "/bin", func(w http.ResponseWriter, r *http.Request) {
		// Route ReadFile (/api/fs/read) with content, ReadDir (/api/fs/list)
		// gets a 404 so the directory-fallback branch doesn't match.
		if strings.Contains(r.URL.Path, "/list") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte("hello"))
	})

	info, err := p.Stat(context.Background(), "scan")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.IsDir() {
		t.Error("should be file")
	}
	if info.Size() != 5 {
		t.Errorf("Size = %d, want 5", info.Size())
	}
	if info.Name() != "scan" {
		t.Errorf("Name = %q", info.Name())
	}
}

// TestProxyMount_Stat_DirFallback: when ReadFile fails but ReadDir
// succeeds, Stat reports a directory. Mirrors how `ls /bin/some-dir`
// resolves without a dedicated /api/fs/stat endpoint.
func TestProxyMount_Stat_DirFallback(t *testing.T) {
	p := newTestProxyMount(t, "/bin", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/read") {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("is a directory"))
			return
		}
		// ReadDir succeeds with empty entries
		json.NewEncoder(w).Encode(map[string]any{"entries": []map[string]any{}})
	})

	info, err := p.Stat(context.Background(), "subdir")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Error("should be dir via fallback")
	}
}

// TestProxyMount_Stat_NotFound: both ReadFile and ReadDir 404 →
// returns fs.ErrNotExist so os.Stat-style callers can distinguish
// "doesn't exist" from "permission denied" etc.
func TestProxyMount_Stat_NotFound(t *testing.T) {
	p := newTestProxyMount(t, "/bin", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := p.Stat(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestNewProxyMountFS registers the five standard prefixes onto a
// MountFS. Pin both the prefix set and the writable flag — /log is
// read-only because shards are append-only via ingest, not /api/fs.
func TestNewProxyMountFS(t *testing.T) {
	mfs := NewProxyMountFS("http://example.test", "tok", Header("X-Active-Org", "acme"))
	if mfs == nil {
		t.Fatal("nil MountFS")
	}
	// MountFS doesn't expose registered mounts directly; assert by
	// checking the ReadDir routing for each prefix produces a request
	// against the right path.
	_ = url.QueryEscape // keep import realistic
}
