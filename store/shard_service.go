package store

// ShardService is one service a shard serves (migration 058), stamped
// at christen from the shard's bucket and frozen thereafter. namespace_id
// is the customer-scoped namespaces.id; service is the per-namespace
// service identifier (log_groups.app).
type ShardService struct {
	ID          int64  `gorm:"primaryKey;column:id"`
	ShardID     string `gorm:"not null;column:shard_id"`
	NamespaceID uint   `gorm:"not null;column:namespace_id"` // → namespaces.id (matches the other NamespaceID FKs)
	Service     string `gorm:"not null;column:service"`
}

func (ShardService) TableName() string { return "shard_services" }

// ShardIDsServingService returns the set of shard IDs that serve the
// given (namespaceID, service), read from the christen-time stamp in
// shard_services. An empty service narrows to the namespace only (every
// shard serving any service in that namespace) — the wildcard a
// namespace-level query (/log/<ns>) needs. The result is a set for O(1)
// membership tests at the registry filter; callers treat a nil/empty
// map as "no shard serves this", which routes to zero shards.
func (s *Store) ShardIDsServingService(namespaceID uint, service string) (map[string]struct{}, error) {
	q := s.db.Model(&ShardService{}).
		Distinct("shard_id").
		Where("namespace_id = ?", namespaceID)
	if service != "" {
		q = q.Where("service = ?", service)
	}
	var ids []string
	if err := q.Pluck("shard_id", &ids).Error; err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out, nil
}

// StampShardServices snapshots the services of bucketID onto shardID —
// the permanent christen-time stamp. The services are read from the
// canonical registry (log_groups), where a NULL bucket_id means the
// default bucket (resolved via the label lookup), so this works for
// both the default and dedicated buckets. Idempotent.
func (s *Store) StampShardServices(shardID string, bucketID int64) error {
	return s.db.Exec(`
		INSERT INTO shard_services (shard_id, namespace_id, service)
		SELECT ?, lg.namespace_id, lg.app
		FROM log_groups lg
		WHERE COALESCE(lg.bucket_id, (SELECT id FROM buckets WHERE label = 'default')) = ?
		  AND lg.deleted_at IS NULL
		ON CONFLICT (shard_id, namespace_id, service) DO NOTHING`,
		shardID, bucketID).Error
}
