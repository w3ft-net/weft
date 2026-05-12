package mounts

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/weft/store"
)

type CronMount struct{ cfg Config }

func (m *CronMount) Prefix() string { return "/cron.d" }

func (m *CronMount) Caps() shellcore.MountCaps {
	return shellcore.MountCaps{Kind: shellcore.MountLocal, Writable: true}
}

func (m *CronMount) ReadDir(rel string) ([]fs.DirEntry, error) {
	if rel != "" {
		return nil, fs.ErrNotExist
	}
	watches, err := m.cfg.Store.ListCronWatches(m.cfg.CustomerID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(watches))
	for _, w := range watches {
		if w.CreatedByUserID != nil {
			ids = append(ids, *w.CreatedByUserID)
		}
	}
	owners := resolveOwners(m.cfg.Store, ids)
	entries := make([]fs.DirEntry, len(watches))
	for i, w := range watches {
		owner := ""
		if w.CreatedByUserID != nil {
			owner = owners[*w.CreatedByUserID]
		}
		entries[i] = &vfsDirEntry{
			name:  w.Name,
			size:  int64(len(SerializeCron(&w))),
			mtime: w.UpdatedAt,
			owner: owner,
		}
	}
	return entries, nil
}

func (m *CronMount) ReadFile(rel string) ([]byte, error) {
	w, err := m.cfg.Store.GetCronWatch(m.cfg.CustomerID, rel)
	if err != nil {
		return nil, fmt.Errorf("/cron.d/%s: no such file", rel)
	}
	return []byte(SerializeCron(w)), nil
}

func (m *CronMount) Stat(rel string) (fs.FileInfo, error) {
	if rel == "" {
		return &vfsFileInfo{name: "cron.d", dir: true}, nil
	}
	w, err := m.cfg.Store.GetCronWatch(m.cfg.CustomerID, rel)
	if err != nil {
		return nil, fs.ErrNotExist
	}
	return &vfsFileInfo{name: rel, size: int64(len(SerializeCron(w))), mtime: w.UpdatedAt}, nil
}

func (m *CronMount) WriteFile(rel string, data []byte) error {
	if !m.cfg.IsOwner && !m.cfg.CanManageWatches {
		return denyWrite("/cron.d")
	}
	schedule, pipeline, err := ParseCron(string(data))
	if err != nil {
		return fmt.Errorf("/cron.d/%s: %w", rel, err)
	}

	maxWatches := store.WatchLimit(m.cfg.Tier)
	if maxWatches == 0 {
		return fmt.Errorf("watches are not available on the trial plan")
	}

	if _, err := m.cfg.Store.GetCronWatch(m.cfg.CustomerID, rel); err != nil {
		count, err := m.cfg.Store.CountCronWatches(m.cfg.CustomerID)
		if err != nil {
			return fmt.Errorf("failed to check watch count: %w", err)
		}
		if count >= maxWatches {
			return fmt.Errorf("watch limit reached (%d watches on %s plan)", maxWatches, m.cfg.Tier)
		}
	}

	minInterval := store.MinWatchInterval(m.cfg.Tier)
	interval := store.ParseScheduleInterval(schedule)
	if interval < minInterval {
		return fmt.Errorf("minimum schedule interval for %s plan is %s", m.cfg.Tier, minInterval)
	}

	return m.cfg.Store.SaveCronWatch(m.cfg.CustomerID, m.cfg.UserID, rel, schedule, pipeline)
}

func (m *CronMount) Remove(rel string) error {
	if !m.cfg.IsOwner && !m.cfg.CanManageWatches {
		return denyWrite("/cron.d")
	}
	return m.cfg.Store.DeleteCronWatch(m.cfg.CustomerID, rel)
}

// AmendVersionMessage allows recording a commit message for cron entries.
func (m *CronMount) AmendVersionMessage(rel, message string) error {
	if !m.cfg.IsOwner && !m.cfg.CanManageWatches {
		return denyWrite("/cron.d")
	}
	return m.cfg.Store.AmendCronWatchMessage(m.cfg.CustomerID, rel, "HEAD", message)
}

// --- Cron serialization (canonical, single source of truth) ---

func SerializeCron(w *store.CronWatch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# schedule: %s\n", w.Schedule)
	if !w.Enabled {
		b.WriteString("# enabled: false\n")
	}
	b.WriteString(w.Pipeline)
	if !strings.HasSuffix(w.Pipeline, "\n") {
		b.WriteByte('\n')
	}
	return b.String()
}

func ParseCron(content string) (schedule, pipeline string, err error) {
	var pipelineLines []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# schedule:") {
			schedule = strings.TrimSpace(strings.TrimPrefix(trimmed, "# schedule:"))
		} else if strings.HasPrefix(trimmed, "# enabled:") {
			// Ignored on write.
		} else if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			pipelineLines = append(pipelineLines, line)
		}
	}
	pipeline = strings.Join(pipelineLines, "\n")
	if schedule == "" {
		err = fmt.Errorf("missing '# schedule:' directive")
	}
	if pipeline == "" {
		err = fmt.Errorf("missing pipeline")
	}
	return
}
