package shard

import (
	"testing"

	"github.com/graybeardcorp/weft/shape"
)

func columnTypes(t *testing.T, ds *DuckDBShard, table string) map[string]string {
	t.Helper()
	rows, err := ds.db.Query(`SELECT name, type FROM pragma_table_info('` + table + `')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var n, ty string
		if err := rows.Scan(&n, &ty); err != nil {
			t.Fatal(err)
		}
		got[n] = ty
	}
	return got
}

// The logs table's partition columns come from the layout, with the layout's
// types, and the default layout is today's customer_id BIGINT.
func TestNewDuckDBShard_PartitionColumnsFollowLayout(t *testing.T) {
	def, err := NewDuckDBShard("def", "")
	if err != nil {
		t.Fatal(err)
	}
	defer def.Close()
	if got := columnTypes(t, def, "logs")["customer_id"]; got != "BIGINT" {
		t.Fatalf("default layout: customer_id = %q, want BIGINT", got)
	}

	lay := shape.NewLayout(
		shape.LayoutColumn{Name: "account_id", Type: shape.TypeInt64, Role: shape.RolePartition},
		shape.LayoutColumn{Name: "tier", Type: shape.TypeString, Role: shape.RolePartition},
	)
	ds, err := NewDuckDBShard("custom", "", WithLayout(lay))
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	cols := columnTypes(t, ds, "logs")
	if cols["account_id"] != "BIGINT" || cols["tier"] != "VARCHAR" {
		t.Errorf("account_id=%q tier=%q, want BIGINT and VARCHAR", cols["account_id"], cols["tier"])
	}
	if _, ok := cols["customer_id"]; ok {
		t.Error("customer_id present in a layout that does not name it")
	}
}
