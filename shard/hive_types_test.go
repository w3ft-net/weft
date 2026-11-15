package shard

import (
	"testing"

	"github.com/w3ft-net/weft/shape"
)

// The legacy layout reads archives exactly as before layouts existed.
func TestParquetReadExpr_LegacyIsUnchanged(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"s3://b/x.parquet", "read_parquet('s3://b/x.parquet')"},
		{"s3://b/x/m60/**/*.parquet", "read_parquet('s3://b/x/m60/**/*.parquet', hive_partitioning=true)"},
		{"s3://b/x/k60/**/*.parquet", "read_parquet('s3://b/x/k60/**/*.parquet', hive_partitioning=true, hive_types={'customer_id': 'BIGINT', 'namespace': 'VARCHAR', 'service': 'VARCHAR', 'kind': 'VARCHAR'})"},
		{"s3://b/x/sized/**/*.parquet", "read_parquet('s3://b/x/sized/**/*.parquet', hive_partitioning=true, hive_types={'customer_id': 'BIGINT', 'namespace': 'VARCHAR', 'service': 'VARCHAR', 'kind': 'VARCHAR', '_w': 'BIGINT'})"},
	} {
		if got := ParquetReadExpr(tc.path, shape.Legacy()); got != tc.want {
			t.Errorf("ParquetReadExpr(%q) =\n %s\nwant\n %s", tc.path, got, tc.want)
		}
	}
}

// Any other layout types its own partition columns, and a layout with none
// types only the key.
func TestParquetReadExpr_TypesTheLayoutsColumns(t *testing.T) {
	two := shape.NewLayout(
		shape.LayoutColumn{Name: "account_id", Type: shape.TypeInt64, Role: shape.RolePartition},
		shape.LayoutColumn{Name: "tier", Type: shape.TypeString, Role: shape.RolePartition},
	)
	got := ParquetReadExpr("s3://b/x/sized/**/*.parquet", two)
	want := "read_parquet('s3://b/x/sized/**/*.parquet', hive_partitioning=true, hive_types={'account_id': 'BIGINT', 'tier': 'VARCHAR', 'namespace': 'VARCHAR', 'service': 'VARCHAR', 'kind': 'VARCHAR', '_w': 'BIGINT'})"
	if got != want {
		t.Errorf("two columns:\n %s\nwant\n %s", got, want)
	}
	got = ParquetReadExpr("s3://b/x/sized/**/*.parquet", shape.NewLayout())
	want = "read_parquet('s3://b/x/sized/**/*.parquet', hive_partitioning=true, hive_types={'namespace': 'VARCHAR', 'service': 'VARCHAR', 'kind': 'VARCHAR', '_w': 'BIGINT'})"
	if got != want {
		t.Errorf("no columns:\n %s\nwant\n %s", got, want)
	}
}
