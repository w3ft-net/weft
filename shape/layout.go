package shape

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
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

// Parse converts a batch's positional partition values to the layout's column
// types, once per batch. It refuses a count that differs from the layout's.
func (l Layout) Parse(values []string) ([]any, error) {
	if len(values) != len(l.Columns) {
		return nil, fmt.Errorf("layout %s has %d partition columns, batch carries %d values", l.Fingerprint, len(l.Columns), len(values))
	}
	out := make([]any, len(values))
	for i, c := range l.Columns {
		if c.Type != TypeInt64 {
			out[i] = values[i]
			continue
		}
		n, err := strconv.ParseInt(values[i], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("partition column %s: %w", c.Name, err)
		}
		out[i] = n
	}
	return out, nil
}

// IsZero reports whether no layout was stated. It is not the same as a layout
// with no partition columns, which is a real one (a build with no tenant) and
// has a fingerprint. A reader treats a zero layout as the legacy one.
func (l Layout) IsZero() bool { return l.Fingerprint == "" }

// OrLegacy is the layout, or the legacy one when none was stated.
func (l Layout) OrLegacy() Layout {
	if l.IsZero() {
		return Legacy()
	}
	return l
}

// IsLegacy reports whether the layout is the legacy customer_id one.
func (l Layout) IsLegacy() bool { return l.Fingerprint == Legacy().Fingerprint }

// Legacy is the layout every shard volume written before layouts existed has:
// a customer_id BIGINT column. The hosted shape reproduces it exactly.
func Legacy() Layout {
	return NewLayout(LayoutColumn{Name: "customer_id", Type: TypeInt64, Role: RolePartition})
}
