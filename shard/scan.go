package shard

import (
	"database/sql"
	"fmt"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// ScanResultRows reads all rows from a SQL result set that returns
// (line_num, raw) and converts them to ResultRow.
func ScanResultRows(sqlRows *sql.Rows) ([]*pb.ResultRow, error) {
	var results []*pb.ResultRow
	for sqlRows.Next() {
		var lineNum int64
		var raw string
		if err := sqlRows.Scan(&lineNum, &raw); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		results = append(results, &pb.ResultRow{
			LineNum: lineNum,
			Raw:     raw,
		})
	}
	return results, sqlRows.Err()
}
