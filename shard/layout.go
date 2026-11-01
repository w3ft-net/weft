package shard

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/graybeardcorp/weft/logtable"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// A shard keeps its rows in one of two layouts.
//
// In the legacy layout every row is in the logs table.
//
// In the per-key layout each (customer, namespace, service, kind) has a table
// of its own, named by logtable.Name, and logs stays as an empty template: it
// is where migrations land, what new tables are made from, and what a query
// for a key with no rows reads. log_tables records which keys have a table.
// A query names its table with a hint (see resolveSource); one with no hint
// reads every table.
//
// A database picks its layout once. A file that has log_tables is per-key, one
// that holds rows in logs is legacy, and only an empty database opened
// WithPerKeyTables becomes per-key. See docs/plans/per-service-tables.md.

// Option configures NewDuckDBShard.
type Option func(*openOptions)

type openOptions struct {
	perKey bool
}

// WithPerKeyTables makes a database that holds no rows use the per-key layout.
// A database that already has rows, or already has the layout, keeps it.
func WithPerKeyTables() Option {
	return func(o *openOptions) { o.perKey = true }
}

const logTablesSQL = `CREATE TABLE IF NOT EXISTS log_tables (
	table_name TEXT, customer_id BIGINT, namespace TEXT, service TEXT, kind TEXT)`

// tableKey is the key a table holds. kind is already defaulted ("main").
type tableKey struct {
	customerID int64
	namespace  string
	service    string
	kind       string
}

func (k tableKey) table() string {
	return logtable.Name(k.customerID, k.namespace, k.service, k.kind)
}

func rowKey(r *pb.LogRow) tableKey {
	kind := r.Kind
	if kind == "" {
		kind = "main"
	}
	return tableKey{r.CustomerId, r.Namespace, r.Service, kind}
}

// PerKey reports whether the shard keeps a table per key.
func (s *DuckDBShard) PerKey() bool { return s.perKey }

// initLayout settles which layout db has and loads the tables it knows. It
// runs once, after the logs template exists and has been migrated.
func (s *DuckDBShard) initLayout(wantPerKey bool) error {
	has, err := tableExists(s.db, "log_tables")
	if err != nil {
		return err
	}
	if !has {
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
		if _, err := s.db.Exec(logTablesSQL); err != nil {
			return fmt.Errorf("create log_tables: %w", err)
		}
	}
	s.perKey = true
	s.tables = map[string]struct{}{}
	rows, err := s.db.Query(`SELECT table_name FROM log_tables`)
	if err != nil {
		return fmt.Errorf("read log_tables: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		s.tables[name] = struct{}{}
	}
	return rows.Err()
}

func tableExists(db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = ?`, name).Scan(&n)
	return n > 0, err
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
func (s *DuckDBShard) ensureTables(ctx context.Context, keys []tableKey) error {
	for _, k := range keys {
		name := k.table()
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
	// column types match exactly. The table and its log_tables row are one
	// transaction: a table with no row would be unreachable by hint, a row with
	// no table would break every unhinted query.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create table for %v: %w", k, err)
	}
	ddl := strings.Replace(createTableSQL, "IF NOT EXISTS logs", fmt.Sprintf(`IF NOT EXISTS "%s"`, name), 1)
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		tx.Rollback()
		return fmt.Errorf("create table for %v: %w", k, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO log_tables VALUES (?, ?, ?, ?, ?)`, name, k.customerID, k.namespace, k.service, k.kind); err != nil {
		tx.Rollback()
		return fmt.Errorf("record table for %v: %w", k, err)
	}
	if err := tx.Commit(); err != nil {
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
