package shard

import (
	"database/sql"
	"fmt"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// ScanResultRows reads all rows from a SQL result set and converts
// them to ResultRow. Expects (raw, ts) as the first two columns; any
// columns beyond those are carried into ResultRow.MetaVars keyed by
// column name — the planner adds these (MetaVarColumns, in
// planner/jq.go) only when a remaining jq compute stage might read a
// bare "._"-prefixed meta-variable for output, so the common (raw,
// ts)-only shape allocates nothing extra.
func ScanResultRows(sqlRows *sql.Rows) ([]*pb.ResultRow, error) {
	cols, err := sqlRows.Columns()
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}
	extraCols := cols[2:]

	var results []*pb.ResultRow
	for sqlRows.Next() {
		var raw string
		var ts time.Time
		dest := make([]any, 2+len(extraCols))
		dest[0], dest[1] = &raw, &ts
		extraVals := make([]sql.NullString, len(extraCols))
		for i := range extraVals {
			dest[2+i] = &extraVals[i]
		}
		if err := sqlRows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		row := &pb.ResultRow{
			Raw:      raw,
			TsUnixNs: ts.UnixNano(),
		}
		for i, col := range extraCols {
			if !extraVals[i].Valid {
				continue
			}
			if row.MetaVars == nil {
				row.MetaVars = make(map[string]string, len(extraCols))
			}
			row.MetaVars[col] = extraVals[i].String
		}
		results = append(results, row)
	}
	return results, sqlRows.Err()
}
