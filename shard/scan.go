package shard

import (
	"database/sql"
	"fmt"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// ScanResultRows reads all rows from a SQL result set and converts
// them to ResultRow. Expects (raw, ts) columns.
func ScanResultRows(sqlRows *sql.Rows) ([]*pb.ResultRow, error) {
	var results []*pb.ResultRow
	for sqlRows.Next() {
		var raw string
		var ts time.Time
		if err := sqlRows.Scan(&raw, &ts); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		results = append(results, &pb.ResultRow{
			Raw:      raw,
			TsUnixNs: ts.UnixNano(),
		})
	}
	return results, sqlRows.Err()
}
