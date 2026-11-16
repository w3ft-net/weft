package live

import (
	"encoding/json"
	"strconv"
	"time"
)

// PartitionStat summarizes one partition's rows within an archived shard: the
// timestamp span and row count of the rows that share one combination of
// partition column values. Captured during Archive (while the DuckDB handle is
// local on the shard node), recorded in the export manifest, and handed to the
// deployment's retention feature, if it has one, so it can classify archived
// parquet without re-scanning S3.
type PartitionStat struct {
	// PartitionValues are the group's partition column values as text, in
	// layout order. A missing value counts as 0 (or '' for text), as the
	// legacy query always did.
	PartitionValues []string `json:"partition_values"`
	// LegacyCustomerID is read from a stat written before partition values,
	// which named the legacy layout's customer_id alone: an export manifest in S3
	// or a compactor result from an older build. It is never written.
	LegacyCustomerID int64     `json:"customer_id,omitempty"`
	MinTs            time.Time `json:"min_ts"`
	MaxTs            time.Time `json:"max_ts"`
	RowCount         int64     `json:"row_count"`
}

// normalize gives a stat from a shard that predates partition values, which
// named the legacy layout's customer_id alone, the values a current one carries.
// A stat with neither is left as it is: it is what a layout with no partition
// columns produces, and it is not guessed to be customer 0. (An old shard's rows
// with no customer were counted under customer 0 and read back as no values; a
// deployment whose layout has a column treats a stat with no values as the group
// of missing values.)
func (s *PartitionStat) normalize() {
	if len(s.PartitionValues) == 0 && s.LegacyCustomerID != 0 {
		s.PartitionValues = []string{strconv.FormatInt(s.LegacyCustomerID, 10)}
	}
}

// UnmarshalJSON reads a stat written by this build or by an older one,
// which carried customer_id and no partition_values.
func (s *PartitionStat) UnmarshalJSON(b []byte) error {
	type plain PartitionStat
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*s = PartitionStat(p)
	s.normalize()
	return nil
}
