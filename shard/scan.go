package shard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// scanRows reads all rows from a SQL result set and converts them to LogRow.
// Shared by DuckDBShard.Query and ColdShard.Query.
func scanRows(sqlRows *sql.Rows) ([]*pb.LogRow, error) {
	var results []*pb.LogRow
	for sqlRows.Next() {
		var (
			lineNum            int64
			raw, level, app    string
			instance, customer string
			ts                 time.Time
			rawFields          any
			rawPayload         any
		)

		err := sqlRows.Scan(&lineNum, &raw, &rawFields, &ts, &level, &app, &instance, &customer, &rawPayload)
		if err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}

		r := &pb.LogRow{
			LineNum:  lineNum,
			Raw:      raw,
			TsUnixNs: ts.UnixNano(),
			Level:    level,
			App:      app,
			Instance: instance,
			Customer: customer,
		}

		if arr, ok := rawFields.([]any); ok {
			for _, v := range arr {
				if sv, ok := v.(string); ok {
					r.Fields = append(r.Fields, sv)
				}
			}
		}
		if rawPayload != nil {
			b, err := json.Marshal(rawPayload)
			if err == nil {
				r.PayloadJson = b
			}
		}
		results = append(results, r)
	}
	return results, sqlRows.Err()
}
