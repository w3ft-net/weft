package store

import "time"

// ObjectVersion is one saved version of a /bin script or /cron.d entry, as a
// flat row so a caller can assemble an org-wide history in one pass. It
// covers both live rows (IsHead) and history rows.
type ObjectVersion struct {
	Kind      string // "bin", "cron.d", "etc/dashboards" (name is "<dashboard>.yaml") or "etc" (only notify.yaml)
	ObjectID  uint64 // shell_functions.id / cron_watches.id; stable across renames
	Name      string // the object's name at the time of this version
	VersionID int64  // unique per content change; stable when HEAD moves to history
	Source    string // bin: the script source; etc: the file content
	Language  string // bin: "bash" or "lua"
	Schedule  string // cron
	Pipeline  string // cron
	Message   string
	// AuthoredAt is when this version was written, AuthoredBy who wrote it.
	AuthoredAt time.Time
	AuthoredBy *string
	IsHead     bool
	// DeletedAt is when this version was deleted: set on a HEAD row whose
	// object is deleted now, and on a history row whose object was deleted
	// and then saved again.
	DeletedAt *time.Time
	// Truncated marks the oldest retained version of an object whose
	// earlier versions were trimmed by the 5-version history cap.
	Truncated bool
}

// ListObjectVersions returns every retained version of every /bin script and
// /cron.d entry in the org, including soft-deleted objects, ordered by when
// each version was written.
func (s *Store) ListObjectVersions(orgID OrgID) ([]ObjectVersion, error) {
	var out []ObjectVersion
	err := s.db.Raw(`
		SELECT 'bin' AS kind, h.function_id AS object_id, h.name, h.version_id,
		       h.source, h.language, '' AS schedule, '' AS pipeline, h.message,
		       h.authored_at, h.authored_by_user_id AS authored_by, false AS is_head,
		       h.deleted_at,
		       (h.predecessor_id IS NULL AND f.origin_version_id IS DISTINCT FROM h.version_id) AS truncated
		  FROM shell_function_history h JOIN shell_functions f ON f.id = h.function_id
		 WHERE h.org_id = @org
		UNION ALL
		SELECT 'bin', f.id, f.name, f.version_id, f.source, f.language, '', '', f.message,
		       f.authored_at, f.authored_by_user_id, true, f.deleted_at, false
		  FROM shell_functions f WHERE f.org_id = @org
		UNION ALL
		SELECT 'cron.d', h.watch_id, COALESCE(n.name || '/', '') || h.name, h.version_id, '', '', h.schedule, h.pipeline, h.message,
		       h.authored_at, h.authored_by_user_id, false, h.deleted_at,
		       (h.predecessor_id IS NULL AND w.origin_version_id IS DISTINCT FROM h.version_id)
		  FROM cron_watch_history h JOIN cron_watches w ON w.id = h.watch_id
		  LEFT JOIN namespaces n ON n.id = w.namespace_id
		 WHERE h.org_id = @org
		UNION ALL
		SELECT 'cron.d', w.id, COALESCE(n.name || '/', '') || w.name, w.version_id, '', '', w.schedule, w.pipeline, w.message,
		       w.authored_at, w.authored_by_user_id, true, w.deleted_at, false
		  FROM cron_watches w LEFT JOIN namespaces n ON n.id = w.namespace_id WHERE w.org_id = @org
		UNION ALL
		SELECT 'etc/dashboards', h.dashboard_id, h.name || '.yaml', h.version_id, h.yaml_body, '', '', '', h.message,
		       h.authored_at, h.authored_by_user_id, false, NULL::timestamptz,
		       (h.predecessor_id IS NULL AND d.origin_version_id IS DISTINCT FROM h.version_id)
		  FROM dashboard_history h JOIN dashboards d ON d.id = h.dashboard_id
		 WHERE h.org_id = @org
		UNION ALL
		SELECT 'etc/dashboards', d.id, d.name || '.yaml', d.version_id, d.yaml_body, '', '', '', d.message,
		       d.authored_at, d.authored_by_user_id, true, d.deleted_at, false
		  FROM dashboards d WHERE d.org_id = @org
		UNION ALL
		-- The first row of a notify config can be created empty (Slack setup
		-- makes it); that is no version of the file.
		SELECT 'etc', h.notify_config_id, 'notify.yaml', h.version_id, h.aliases_yaml, '', '', '', h.message,
		       h.authored_at, h.authored_by_user_id, false, NULL::timestamptz,
		       (h.predecessor_id IS NULL AND n.origin_version_id IS DISTINCT FROM h.version_id)
		  FROM notify_config_history h JOIN notify_configs n ON n.id = h.notify_config_id
		 WHERE h.org_id = @org AND NOT (h.aliases_yaml = '' AND h.version_id = n.origin_version_id)
		UNION ALL
		SELECT 'etc', n.id, 'notify.yaml', n.version_id, n.aliases_yaml, '', '', '', n.message,
		       n.authored_at, n.authored_by_user_id, true, n.deleted_at, false
		  FROM notify_configs n
		 WHERE n.org_id = @org AND NOT (n.aliases_yaml = '' AND n.version_id = n.origin_version_id)
		ORDER BY authored_at, kind, object_id, version_id`,
		map[string]any{"org": orgID}).Scan(&out).Error
	return out, err
}
