package shell

import (
	"fmt"
	"strings"
)

// MemBackend implements Backend with in-memory storage (for dev/test).
type MemBackend struct {
	files map[string]string // keyed by "bin/<name>" or "cron.d/<name>"
}

// NewMemBackend creates an empty in-memory backend.
func NewMemBackend() *MemBackend {
	return &MemBackend{files: make(map[string]string)}
}

func (b *MemBackend) ListFunctions() ([]string, error) {
	return b.listPrefix("bin/"), nil
}

func (b *MemBackend) GetFunction(name string) (string, error) {
	if src, ok := b.files["bin/"+name]; ok {
		return src, nil
	}
	return "", fmt.Errorf("not found: %s", name)
}

func (b *MemBackend) SaveFunction(name, source string) error {
	b.files["bin/"+name] = source
	return nil
}

func (b *MemBackend) DeleteFunction(name string) error {
	key := "bin/" + name
	if _, ok := b.files[key]; !ok {
		return fmt.Errorf("not found: %s", name)
	}
	delete(b.files, key)
	return nil
}

func (b *MemBackend) ListWatches() ([]string, error) {
	return b.listPrefix("cron.d/"), nil
}

func (b *MemBackend) GetWatch(name string) (*WatchData, error) {
	content, ok := b.files["cron.d/"+name]
	if !ok {
		return nil, fmt.Errorf("not found: %s", name)
	}
	schedule, pipeline := parseCronFileContent(content)
	return &WatchData{
		Schedule: schedule,
		Pipeline: pipeline,
		Enabled:  true,
	}, nil
}

func (b *MemBackend) SaveWatch(name string, w *WatchData) error {
	b.files["cron.d/"+name] = serializeCronFile(w)
	return nil
}

func (b *MemBackend) DeleteWatch(name string) error {
	key := "cron.d/" + name
	if _, ok := b.files[key]; !ok {
		return fmt.Errorf("not found: %s", name)
	}
	delete(b.files, key)
	return nil
}

func (b *MemBackend) ListDashboards() ([]string, error) {
	return b.listPrefix("etc/dashboards/"), nil
}

func (b *MemBackend) GetDashboard(name string) (string, error) {
	if body, ok := b.files["etc/dashboards/"+name]; ok {
		return body, nil
	}
	return "", fmt.Errorf("not found: %s", name)
}

func (b *MemBackend) SaveDashboard(name, body string) error {
	b.files["etc/dashboards/"+name] = body
	return nil
}

func (b *MemBackend) DeleteDashboard(name string) error {
	key := "etc/dashboards/" + name
	if _, ok := b.files[key]; !ok {
		return fmt.Errorf("not found: %s", name)
	}
	delete(b.files, key)
	return nil
}

func (b *MemBackend) GetNotifyConfig() (string, error) {
	return b.files["etc/notify.conf"], nil
}

func (b *MemBackend) SaveNotifyConfig(content string) error {
	b.files["etc/notify.conf"] = content
	return nil
}

func (b *MemBackend) listPrefix(prefix string) []string {
	var names []string
	for k := range b.files {
		if strings.HasPrefix(k, prefix) {
			name := k[len(prefix):]
			if !strings.Contains(name, "/") {
				names = append(names, name)
			}
		}
	}
	return names
}
