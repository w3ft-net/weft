package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/w3ft-net/weft/backends/dashboard"
	"github.com/w3ft-net/weft/shape"
)

// DashboardBackend is a dashboard.Store over the install's SQLite database.
type DashboardBackend struct {
	db  *sql.DB
	now func() time.Time
}

var _ dashboard.Store = (*DashboardBackend)(nil)

const dashboardSchema = `
CREATE TABLE IF NOT EXISTS dashboards (
	name TEXT PRIMARY KEY,
	yaml_body TEXT NOT NULL,
	created_by TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
`

// NewDashboardBackend is a store over db, creating its table if need be. db comes
// from sqlitedb.Open and is the caller's to close.
func NewDashboardBackend(db *sql.DB) (*DashboardBackend, error) {
	if _, err := db.Exec(dashboardSchema); err != nil {
		return nil, fmt.Errorf("create dashboards schema: %w", err)
	}
	return &DashboardBackend{db: db, now: time.Now}, nil
}

// Times are stored as fixed-width UTC text.
const dashboardTimeFmt = "2006-01-02T15:04:05.000000000Z"

const dashboardCols = `name, yaml_body, created_by, created_at, updated_at`

type dashboardScanner interface{ Scan(dest ...any) error }

func dashboardScan(r dashboardScanner) (*dashboard.Dashboard, error) {
	var (
		d                dashboard.Dashboard
		created, updated string
	)
	if err := r.Scan(&d.Name, &d.YAMLBody, &d.CreatedBy, &created, &updated); err != nil {
		return nil, err
	}
	var err error
	if d.CreatedAt, err = time.Parse(dashboardTimeFmt, created); err != nil {
		return nil, err
	}
	if d.UpdatedAt, err = time.Parse(dashboardTimeFmt, updated); err != nil {
		return nil, err
	}
	return &d, nil
}

// Save is one statement: an upsert that leaves the row alone when the body is
// unchanged.
func (s *DashboardBackend) Save(_ shape.Added, user, name, yamlBody string) error {
	now := s.now().UTC().Format(dashboardTimeFmt)
	_, err := s.db.Exec(`INSERT INTO dashboards (name, yaml_body, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET yaml_body = excluded.yaml_body, updated_at = excluded.updated_at
		WHERE dashboards.yaml_body <> excluded.yaml_body`, name, yamlBody, user, now, now)
	return err
}

func (s *DashboardBackend) Get(_ shape.Added, name string) (*dashboard.Dashboard, error) {
	d, err := dashboardScan(s.db.QueryRow(`SELECT `+dashboardCols+` FROM dashboards WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, dashboard.ErrNotFound
	}
	return d, err
}

func (s *DashboardBackend) List(shape.Added) ([]dashboard.Dashboard, error) {
	rows, err := s.db.Query(`SELECT ` + dashboardCols + ` FROM dashboards ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dashboard.Dashboard
	for rows.Next() {
		d, err := dashboardScan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

func (s *DashboardBackend) Delete(_ shape.Added, name string) error {
	_, err := s.db.Exec(`DELETE FROM dashboards WHERE name = ?`, name)
	return err
}
