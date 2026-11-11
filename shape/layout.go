package shape

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Role says what a column is for. Only partition columns own storage.
type Role string

const RolePartition Role = "partition"

// ColumnType is a partition column's storage type. It travels with the layout
// so shards keep BIGINT comparisons and hive pruning instead of widening to text.
type ColumnType string

const (
	TypeInt64  ColumnType = "int64"
	TypeString ColumnType = "string"
)

// SQL is the DuckDB type name for the column.
func (t ColumnType) SQL() string {
	if t == TypeInt64 {
		return "BIGINT"
	}
	return "VARCHAR"
}

// LayoutColumn is one partition column of the storage layout.
type LayoutColumn struct {
	Name string     `json:"name"`
	Type ColumnType `json:"type"`
	Role Role       `json:"role"`
}

// Layout is the wire descriptor of a build's partition columns: what the
// coordinator tells a shard to store, and what the shard records on its volume.
type Layout struct {
	Columns     []LayoutColumn `json:"columns"`
	Fingerprint string         `json:"fingerprint"`
}

// NewLayout builds a layout and derives its fingerprint from the columns.
func NewLayout(cols ...LayoutColumn) Layout {
	var b strings.Builder
	for _, c := range cols {
		fmt.Fprintf(&b, "%s:%s:%s;", c.Name, c.Type, c.Role)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return Layout{Columns: cols, Fingerprint: hex.EncodeToString(sum[:8])}
}

// IsLegacy reports whether the layout is the legacy customer_id one.
func (l Layout) IsLegacy() bool { return l.Fingerprint == Legacy().Fingerprint }

// Legacy is the layout every shard volume written before layouts existed has:
// a customer_id BIGINT column. The hosted shape reproduces it exactly.
func Legacy() Layout {
	return NewLayout(LayoutColumn{Name: "customer_id", Type: TypeInt64, Role: RolePartition})
}
