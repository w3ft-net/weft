package store

import "gorm.io/gorm"

// ShardFieldFact is one TYPED field fact a shard reported (migration 110):
// the locator ("$N") of the (customer, namespace, service, kind, env) source
// held only numbers in every row the shard ingested. Epoch is the shard's own
// ordering of its facts. A dirty or never-typed locator has no row.
type ShardFieldFact struct {
	ShardID    string `gorm:"primaryKey;column:shard_id"`
	CustomerID int64  `gorm:"primaryKey;column:customer_id"`
	Namespace  string `gorm:"primaryKey;column:namespace"`
	Service    string `gorm:"primaryKey;column:service"`
	Kind       string `gorm:"primaryKey;column:kind"`
	Env        string `gorm:"primaryKey;column:env"`
	Locator    string `gorm:"primaryKey;column:locator"`
	Epoch      int64  `gorm:"not null;column:epoch"`
}

func (ShardFieldFact) TableName() string { return "shard_field_facts" }

// ListShardFieldFacts returns every shard's typed facts. The registry loads
// it at boot and on refresh, grouped by shard_id at the caller.
func (s *Store) ListShardFieldFacts() ([]ShardFieldFact, error) {
	var rows []ShardFieldFact
	err := s.db.Find(&rows).Error
	return rows, err
}

// ApplyShardFieldFacts records one heartbeat's changes to shardID's facts in
// a transaction: typed rows are upserted, dropped rows (typed facts that went
// dirty) are deleted. The rows' ShardID is ignored in favor of shardID.
func (s *Store) ApplyShardFieldFacts(shardID string, typed, dropped []ShardFieldFact) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		for _, f := range typed {
			if err := tx.Exec(`
				INSERT INTO shard_field_facts (shard_id, customer_id, namespace, service, kind, env, locator, epoch)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (shard_id, customer_id, namespace, service, kind, env, locator)
				DO UPDATE SET epoch = EXCLUDED.epoch`,
				shardID, f.CustomerID, f.Namespace, f.Service, f.Kind, f.Env, f.Locator, f.Epoch).Error; err != nil {
				return err
			}
		}
		for _, f := range dropped {
			if err := tx.Exec(`
				DELETE FROM shard_field_facts
				WHERE shard_id = ? AND customer_id = ? AND namespace = ? AND service = ?
				  AND kind = ? AND env = ? AND locator = ?`,
				shardID, f.CustomerID, f.Namespace, f.Service, f.Kind, f.Env, f.Locator).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
