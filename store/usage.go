package store

import "time"

// UpsertUsageHourly inserts or increments an hourly usage record.
func (s *Store) UpsertUsageHourly(customerName string, hour time.Time, bytes int64) error {
	return s.db.Exec(`
		INSERT INTO usage_hourly (customer_name, hour, bytes_ingested)
		VALUES (?, ?, ?)
		ON CONFLICT (customer_name, hour) DO UPDATE
		SET bytes_ingested = usage_hourly.bytes_ingested + EXCLUDED.bytes_ingested`,
		customerName, hour, bytes).Error
}

// QueryUsage returns total bytes ingested for a customer in a time range,
// combining both hourly and daily tables.
func (s *Store) QueryUsage(customerName string, periodStart, periodEnd time.Time) (int64, error) {
	var result struct{ Total int64 }
	err := s.db.Raw(`
		SELECT COALESCE(SUM(bytes_ingested), 0) AS total FROM (
			SELECT bytes_ingested FROM usage_hourly
			WHERE customer_name = ? AND hour >= ? AND hour < ?
			UNION ALL
			SELECT bytes_ingested FROM usage_daily
			WHERE customer_name = ? AND day >= ?::date AND day < ?::date
		) combined`,
		customerName, periodStart, periodEnd,
		customerName, periodStart, periodEnd).Scan(&result).Error
	return result.Total, err
}
