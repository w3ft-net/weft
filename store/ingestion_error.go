package store

import (
	"strings"
)

// RecordIngestionError persists an ingest failure for dashboard display.
// Apps and customers are stored as comma-separated TEXT (avoids pq.Array dependency).
func (s *Store) RecordIngestionError(shardID, errorMsg string, rowCount int, apps, customers []string, dlqPath string) error {
	return s.db.Exec(`
		INSERT INTO ingestion_errors (shard_id, error_msg, row_count, apps, customers, dlq_path)
		VALUES (?, ?, ?, ?, ?, ?)`,
		shardID, errorMsg, rowCount,
		"{"+strings.Join(apps, ",")+"}",
		"{"+strings.Join(customers, ",")+"}",
		dlqPath,
	).Error
}

// UnresolvedIngestionApps returns distinct app names with unresolved
// ingestion errors in the last hour.
func (s *Store) UnresolvedIngestionApps() ([]string, error) {
	var apps []string
	err := s.db.Raw(`
		SELECT DISTINCT unnest(apps) AS app
		FROM ingestion_errors
		WHERE resolved_at IS NULL
		AND occurred_at > NOW() - interval '1 hour'
		ORDER BY app`).Scan(&apps).Error
	return apps, err
}

// ResolveIngestionError marks a DLQ error as resolved.
func (s *Store) ResolveIngestionError(dlqPath string) error {
	return s.db.Exec(`
		UPDATE ingestion_errors SET resolved_at = NOW()
		WHERE dlq_path = ? AND resolved_at IS NULL`, dlqPath).Error
}
