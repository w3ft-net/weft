package hosted

import "github.com/graybeardcorp/weft/logtable"

// LegacyFactTable is the logical table of a source identified by customer_id,
// namespace, service and kind alone: what a shard that predates table names
// reports a field fact under, and how the store keeps one. The hosted layout's
// one partition column is the customer, so it is logtable.Name of that.
func LegacyFactTable(customerID int64, namespace, service, kind string) string {
	return logtable.Name([]any{customerID}, namespace, service, kind)
}
