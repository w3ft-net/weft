package shard

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/w3ft-net/weft/logtable"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

// A shard keeps its rows in one of two layouts.
//
// In the legacy layout every row is in the logs table.
//
// In the per-key layout each (customer, namespace, service, kind) has a table
// of its own, named by logtable.Name, and logs stays as an empty template: it
// is where migrations land, what new tables are made from, and what a query
// for a key with no rows reads. A query names its table with a hint (see
// resolveSource); one with no hint reads every table.
//
// A database picks its layout once. A file that has log tables is per-key, one
// that holds rows in logs is legacy, and only an empty database opened
// WithPerKeyTables becomes per-key. See docs/plans/per-service-tables.md.

// Option configures NewDuckDBShard.
type Option func(*openOptions)

type openOptions struct {
	perKey bool
	layout shape.Layout
}

// WithLayout sets the partition columns a new database's tables carry. The
// default is shape.Legacy(). A database that already has its tables keeps them.
func WithLayout(l shape.Layout) Option {
	return func(o *openOptions) { o.layout = l }
}

// WithPerKeyTables makes a database that holds no rows use the per-key layout.
// A database that already has rows, or already has the layout, keeps it.
func WithPerKeyTables() Option {
	return func(o *openOptions) { o.perKey = true }
}

// tableKey is the part of a table's key that the row supplies: its namespace,
// service and kind, the last already defaulted ("main"). The rest of the key is
// the batch's partition. stamped is the customer_id of a row from a coordinator
// that sends no partition values, which the legacy layout reads in its place.
type tableKey struct {
	namespace string
	service   string
	kind      string
	stamped   int64
}

// table is the key's table for a batch stored under partition.
func (k tableKey) table(partition []any) string {
	if partition == nil {
		partition = []any{k.stamped}
	}
	return logtable.Name(partition, k.namespace, k.service, k.kind)
}

func rowKey(partition []any, r *pb.LogRow) tableKey {
	kind := r.Kind
	if kind == "" {
		kind = "main"
	}
	k := tableKey{namespace: r.Namespace, service: r.Service, kind: kind}
	if partition == nil {
		k.stamped = r.CustomerId
	}
	return k
}

// PerKey reports whether the shard keeps a table per key.
func (s *DuckDBShard) PerKey() bool { return s.perKey }

// initLayout settles which layout db has and loads the tables it knows. It
// runs once, after the logs template exists and has been migrated.
func (s *DuckDBShard) initLayout(wantPerKey bool) error {
	tables, err := logTables(s.db)
	if err != nil {
		return err
	}
	if len(tables) == 0 {
		if !wantPerKey {
			return nil
		}
		var n int64
		if err := s.db.QueryRow("SELECT COUNT(*) FROM logs").Scan(&n); err != nil {
			return fmt.Errorf("count logs: %w", err)
		}
		if n > 0 {
			return nil // rows are in logs; this database stays legacy
		}
	}
	s.perKey = true
	s.tables = tables
	return nil
}

// logTables returns the log tables db holds: the tables whose names logtable.Name
// produces.
func logTables(db *sql.DB) (map[string]struct{}, error) {
	rows, err := db.Query(`SELECT table_name FROM duckdb_tables()`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	tables := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if logtable.IsName(name) {
			tables[name] = struct{}{}
		}
	}
	return tables, rows.Err()
}

type columnDef struct{ name, typ string }

func tableColumns(db *sql.DB, table string) ([]columnDef, error) {
	rows, err := db.Query(fmt.Sprintf(`SELECT name, type FROM pragma_table_info('%s')`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []columnDef
	for rows.Next() {
		var c columnDef
		if err := rows.Scan(&c.name, &c.typ); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// syncTableColumns adds to each keyed table the columns the logs template has
// and it lacks, so a column added to the schema reaches tables created before
// it. Tables are never given the legacy migrations: they were created at the
// template's shape.
func (s *DuckDBShard) syncTableColumns() error {
	want, err := tableColumns(s.db, "logs")
	if err != nil {
		return fmt.Errorf("read logs columns: %w", err)
	}
	for _, name := range s.tableNames() {
		have, err := tableColumns(s.db, name)
		if err != nil {
			return fmt.Errorf("read columns of %s: %w", name, err)
		}
		got := map[string]bool{}
		for _, c := range have {
			got[c.name] = true
		}
		for _, c := range want {
			if got[c.name] {
				continue
			}
			if _, err := s.db.Exec(fmt.Sprintf(`ALTER TABLE "%s" ADD COLUMN IF NOT EXISTS %s %s`, name, c.name, c.typ)); err != nil {
				return fmt.Errorf("add %s to %s: %w", c.name, name, err)
			}
		}
	}
	return nil
}

// tableNames returns the keyed tables, sorted so generated SQL is stable.
func (s *DuckDBShard) tableNames() []string {
	s.tablesMu.RLock()
	defer s.tablesMu.RUnlock()
	names := make([]string, 0, len(s.tables))
	for n := range s.tables {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ensureTables creates the table of each key that lacks one.
func (s *DuckDBShard) ensureTables(ctx context.Context, partition []any, keys []tableKey) error {
	for _, k := range keys {
		name := k.table(partition)
		s.tablesMu.RLock()
		_, ok := s.tables[name]
		s.tablesMu.RUnlock()
		if ok {
			continue
		}
		if err := s.createTable(ctx, name, k); err != nil {
			return err
		}
	}
	return nil
}

func (s *DuckDBShard) createTable(ctx context.Context, name string, k tableKey) error {
	s.tablesMu.Lock()
	defer s.tablesMu.Unlock()
	if _, ok := s.tables[name]; ok {
		return nil
	}
	// The table is made from createTableSQL, the shape the appender writes, so
	// column types match exactly.
	ddl := strings.Replace(createTableDDL(s.layout), "IF NOT EXISTS logs", fmt.Sprintf(`IF NOT EXISTS "%s"`, name), 1)
	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("create table for %v: %w", k, err)
	}
	s.tables[name] = struct{}{}
	return nil
}

// source returns what follows FROM to read every row the shard holds: logs in
// the legacy layout, the tables unioned in the per-key one.
func (s *DuckDBShard) source() string {
	if !s.perKey {
		return "logs"
	}
	return s.unionSource()
}

func (s *DuckDBShard) unionSource() string {
	names := s.tableNames()
	switch len(names) {
	case 0:
		return "logs" // the empty template
	case 1:
		return `"` + names[0] + `"`
	}
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = `SELECT * FROM "` + n + `"`
	}
	// BY NAME: tables created at different times can differ in column order.
	return "(" + strings.Join(parts, " UNION ALL BY NAME ") + ")"
}

var fromLogs = regexp.MustCompile(`\bFROM logs\b`)

// resolveSource points a query's `FROM logs` at the table that holds its rows.
// A legacy shard runs the query as written. A per-key shard reads the hinted
// table, or, when the key has no table, the empty template (no rows, same
// columns), or every table when there is no hint.
func (s *DuckDBShard) resolveSource(query, table string) string {
	if !s.perKey {
		return query
	}
	src := s.unionSource()
	if table != "" {
		s.tablesMu.RLock()
		_, ok := s.tables[table]
		s.tablesMu.RUnlock()
		if ok {
			src = `"` + table + `"`
		} else {
			src = "logs"
		}
	}
	return fromLogs.ReplaceAllLiteralString(query, "FROM "+src)
}
