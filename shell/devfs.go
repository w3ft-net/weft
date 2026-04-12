package shell

import (
	"fmt"
	"strings"
)

// DevFS is an in-memory FS for the embedded dev coordinator (no gRPC).
type DevFS struct {
	apps  []string
	files map[string]string
}

func NewDevFS(apps []string) *DevFS {
	return &DevFS{apps: apps, files: make(map[string]string)}
}

func (f *DevFS) ReadFile(path string) ([]byte, error) {
	p := normFSPath(path)
	if content, ok := f.files[p]; ok {
		return []byte(content), nil
	}
	return nil, fmt.Errorf("%s: not found", path)
}

func (f *DevFS) WriteFile(path string, data []byte) error {
	p := normFSPath(path)
	dir, _ := splitFSPath(p)
	if dir == "bin" || dir == "cron.d" {
		f.files[p] = string(data)
		return nil
	}
	return fmt.Errorf("%s: read-only", path)
}

func (f *DevFS) ReadDir(path string) ([]string, error) {
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
	case "/bin", "/cron.d", "/etc":
		prefix := p + "/"
		var entries []string
		for k := range f.files {
			if strings.HasPrefix(k, prefix) {
				name := k[len(prefix):]
				if !strings.Contains(name, "/") {
					entries = append(entries, name)
				}
			}
		}
		return entries, nil
	}
	return nil, fmt.Errorf("%s: no such directory", path)
}

func (f *DevFS) Remove(path string) error {
	p := normFSPath(path)
	if _, ok := f.files[p]; ok {
		delete(f.files, p)
		return nil
	}
	return fmt.Errorf("%s: not found", path)
}
