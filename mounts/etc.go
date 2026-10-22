package mounts

import (
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/store"
)

type EtcMount struct{ cfg Config }

func (m *EtcMount) Prefix() string { return "/etc" }

func (m *EtcMount) Caps() shellcore.MountCaps {
	return shellcore.MountCaps{Writable: true}
}

// Authorize is the mount's one access check, called by MountFS before every
// operation: every member can read /etc, only the configmgr group can change
// it, and /etc/passwd is read-only for everyone — the team is mutated with
// adduser/deluser, not by editing the file.
func (m *EtcMount) Authorize(rel string, want shellcore.Perm) error {
	if want&shellcore.PermWrite == 0 {
		return nil
	}
	if rel == "passwd" {
		return fmt.Errorf("/etc/passwd is read-only — manage the team with adduser/deluser")
	}
	if !m.cfg.CanManageEtc {
		return denyWrite("/etc", "configmgr")
	}
	return nil
}

func (m *EtcMount) ReadDir(rel string) ([]fs.DirEntry, error) {
	if rel == "dashboards" {
		dashboards, err := m.cfg.Store.ListDashboards(m.cfg.OrgID)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(dashboards))
		for _, d := range dashboards {
			if d.CreatedByUserID != nil {
				ids = append(ids, *d.CreatedByUserID)
			}
		}
		owners := resolveOwners(m.cfg.Store, ids)
		entries := make([]fs.DirEntry, len(dashboards))
		for i, d := range dashboards {
			owner := ""
			if d.CreatedByUserID != nil {
				owner = owners[*d.CreatedByUserID]
			}
			entries[i] = &vfsDirEntry{
				name:  d.Name + ".yaml",
				size:  int64(len(d.YAMLBody)),
				mtime: d.UpdatedAt,
				owner: owner,
			}
		}
		return entries, nil
	}
	if rel != "" {
		return nil, fs.ErrNotExist
	}
	var notifySize int64
	var notifyMtime time.Time
	if nc, err := m.cfg.Store.GetNotifyConfig(m.cfg.OrgID); err == nil {
		notifySize = int64(len(store.RedactNotifyYAML(nc.AliasesYAML)))
		notifyMtime = nc.UpdatedAt
	}
	passwdSize, passwdMtime := m.passwdFileInfo()
	return []fs.DirEntry{
		&vfsDirEntry{name: "dashboards", dir: true},
		&vfsDirEntry{name: "notify.yaml", size: notifySize, mtime: notifyMtime},
		&vfsDirEntry{name: "passwd", size: passwdSize, mtime: passwdMtime},
		&vfsDirEntry{name: "usage", mtime: time.Now()},
	}, nil
}

func (m *EtcMount) ReadFile(rel string) ([]byte, error) {
	switch {
	case rel == "notify.yaml":
		nc, err := m.cfg.Store.GetNotifyConfig(m.cfg.OrgID)
		if err != nil {
			return []byte(""), nil
		}
		// Webhook header values are credentials: read them redacted.
		return []byte(store.RedactNotifyYAML(nc.AliasesYAML)), nil
	case rel == "passwd":
		out, err := m.cfg.Store.RenderPasswd(m.cfg.OrgID)
		if err != nil {
			return nil, err
		}
		return []byte(out), nil
	case rel == "usage":
		return []byte(m.buildUsageReport()), nil
	default:
		if dashName, ok := dashboardName(rel); ok {
			d, err := m.cfg.Store.GetDashboard(m.cfg.OrgID, dashName)
			if err != nil {
				return nil, fmt.Errorf("/etc/%s: no such file", rel)
			}
			return []byte(d.YAMLBody), nil
		}
	}
	return nil, fmt.Errorf("/etc/%s: no such file", rel)
}

func (m *EtcMount) Stat(rel string) (fs.FileInfo, error) {
	if rel == "" {
		return &vfsFileInfo{name: "etc", dir: true}, nil
	}
	switch {
	case rel == "notify.yaml":
		if nc, err := m.cfg.Store.GetNotifyConfig(m.cfg.OrgID); err == nil {
			return &vfsFileInfo{name: "notify.yaml", size: int64(len(store.RedactNotifyYAML(nc.AliasesYAML))), mtime: nc.UpdatedAt}, nil
		}
		return &vfsFileInfo{name: "notify.yaml"}, nil
	case rel == "passwd":
		size, mtime := m.passwdFileInfo()
		return &vfsFileInfo{name: "passwd", size: size, mtime: mtime}, nil
	case rel == "usage":
		return &vfsFileInfo{name: "usage", mtime: time.Now()}, nil
	case rel == "dashboards":
		return &vfsFileInfo{name: "dashboards", dir: true}, nil
	default:
		if dashName, ok := dashboardName(rel); ok {
			d, err := m.cfg.Store.GetDashboard(m.cfg.OrgID, dashName)
			if err != nil {
				return nil, fs.ErrNotExist
			}
			return &vfsFileInfo{name: lastSegment(rel), size: int64(len(d.YAMLBody)), mtime: d.UpdatedAt}, nil
		}
	}
	return nil, fs.ErrNotExist
}

func (m *EtcMount) WriteFile(rel string, data []byte) error {
	switch {
	case rel == "notify.yaml":
		// What the user edited is the redacted view: a header still showing
		// the placeholder gets its stored secret back, a new value is kept.
		stored := ""
		if nc, err := m.cfg.Store.GetNotifyConfig(m.cfg.OrgID); err == nil {
			stored = nc.AliasesYAML
		}
		content, err := store.RestoreNotifyRedactions(string(data), stored)
		if err != nil {
			return fmt.Errorf("/etc/notify.yaml: %w", err)
		}
		if err := store.ValidateNotifyAliases(content); err != nil {
			return fmt.Errorf("/etc/notify.yaml: %w", err)
		}
		return m.cfg.Store.SaveNotifyAliases(m.cfg.OrgID, m.cfg.UserID, content)
	default:
		if dashName, ok := dashboardName(rel); ok {
			body := string(data)
			spec, err := store.ParseDashboardYAML(body)
			if err != nil {
				return fmt.Errorf("/etc/%s: %w", rel, err)
			}
			if err := m.cfg.Store.ValidateDashboardWatchRefs(m.cfg.OrgID, spec); err != nil {
				return fmt.Errorf("/etc/%s: %w", rel, err)
			}
			return m.cfg.Store.SaveDashboard(m.cfg.OrgID, m.cfg.UserID, dashName, body)
		}
	}
	return fmt.Errorf("/etc/%s: read-only", rel)
}

func (m *EtcMount) Remove(rel string) error {
	if dashName, ok := dashboardName(rel); ok {
		return m.cfg.Store.DeleteDashboard(m.cfg.OrgID, dashName)
	}
	return fmt.Errorf("/etc/%s: operation not permitted", rel)
}

// AmendVersionMessage records a commit message for the versioned /etc files
// (notify.yaml and the dashboards). It is not a MountFS operation, so it
// checks write access itself.
func (m *EtcMount) AmendVersionMessage(rel, message string) error {
	if err := m.Authorize(rel, shellcore.PermWrite); err != nil {
		return err
	}
	if rel == "notify.yaml" {
		return m.cfg.Store.AmendNotifyAliasesMessage(m.cfg.OrgID, m.cfg.UserID, message)
	}
	if dashName, ok := dashboardName(rel); ok {
		return m.cfg.Store.AmendDashboardMessage(m.cfg.OrgID, m.cfg.UserID, dashName, message)
	}
	return fmt.Errorf("/etc/%s: not a versioned file", rel)
}

// --- passwd ---

// passwdFileInfo returns size + mtime for /etc/passwd. The table is
// computed from store state; mtime is "now" since the perms tables
// don't carry a single canonical mtime.
func (m *EtcMount) passwdFileInfo() (int64, time.Time) {
	out, err := m.cfg.Store.RenderPasswd(m.cfg.OrgID)
	if err != nil {
		return 0, time.Time{}
	}
	return int64(len(out)), time.Now()
}

// --- usage report ---

func (m *EtcMount) buildUsageReport() string {
	var b strings.Builder

	now := time.Now()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)

	b.WriteString("# weft usage report\n")
	fmt.Fprintf(&b, "# period: %s — %s\n", periodStart.Format("2006-01-02"), periodEnd.Format("2006-01-02"))
	fmt.Fprintf(&b, "# tier: %s\n\n", m.cfg.Tier)

	bytesIngested, err := m.cfg.Store.QueryUsage(m.cfg.OrgID, periodStart, periodEnd)
	if err != nil {
		bytesIngested = 0
	}

	tierLimitBytes := store.VolumeAllowance(m.cfg.Tier)

	pct := 0.0
	if tierLimitBytes > 0 {
		pct = float64(bytesIngested) / float64(tierLimitBytes) * 100
	}

	fmt.Fprintf(&b, "ingested:  %s / %s (%.1f%%)\n",
		humanBytes(bytesIngested), humanBytes(tierLimitBytes), pct)

	forecast, err := m.cfg.Store.ForecastMetricsUsage(m.cfg.OrgID)
	if err == nil && forecast.AgentsWithMetrics > 0 {
		fmt.Fprintf(&b, "metrics:   ~%s/mo projected (%d agent%s)\n",
			humanBytes(forecast.ProjectedBytesMonth),
			forecast.AgentsWithMetrics, plural(forecast.AgentsWithMetrics))

		totalProjected := bytesIngested + forecast.ProjectedBytesMonth
		totalPct := float64(totalProjected) / float64(tierLimitBytes) * 100
		fmt.Fprintf(&b, "total:     ~%s projected (%.1f%% of limit)\n",
			humanBytes(totalProjected), totalPct)
	}

	return b.String()
}

func humanBytes(b int64) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%.2f GB", float64(b)/(1024*1024*1024))
	case b >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// --- path helpers ---

func dashboardName(rel string) (string, bool) {
	const prefix = "dashboards/"
	const suffix = ".yaml"
	if !strings.HasPrefix(rel, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(rel, prefix)
	if !strings.HasSuffix(rest, suffix) {
		return "", false
	}
	dn := strings.TrimSuffix(rest, suffix)
	if dn == "" || strings.Contains(dn, "/") {
		return "", false
	}
	return dn, true
}

func lastSegment(p string) string {
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		return p[idx+1:]
	}
	return p
}

// FileLanguage implements shellcore.LangMount: the YAML files under /etc
// (notify.yaml and the dashboards) highlight as YAML; passwd and usage
// are plain text.
func (m *EtcMount) FileLanguage(rel string) string {
	if strings.HasSuffix(rel, ".yaml") {
		return "yaml"
	}
	return ""
}
