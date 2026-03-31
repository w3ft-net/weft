package configstore

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

// Store is a SQLite-backed configuration store for the coordinator.
type Store struct {
	db *sql.DB
}

// Open opens or creates a SQLite database at the given path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open config db: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS customers (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT NOT NULL UNIQUE
		)`,
		`CREATE TABLE IF NOT EXISTS accounts (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			customer_id INTEGER NOT NULL REFERENCES customers(id),
			account_id  TEXT NOT NULL,
			role_arn    TEXT NOT NULL,
			external_id TEXT NOT NULL DEFAULT '',
			UNIQUE(customer_id, account_id)
		)`,
		`CREATE TABLE IF NOT EXISTS log_groups (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id    INTEGER NOT NULL REFERENCES accounts(id),
			app           TEXT NOT NULL,
			log_group     TEXT NOT NULL,
			region        TEXT NOT NULL DEFAULT 'us-east-2',
			schema_yaml   TEXT NOT NULL,
			poll_interval TEXT NOT NULL DEFAULT '30s',
			enabled       INTEGER NOT NULL DEFAULT 1,
			last_poll_at  TEXT,
			last_token    TEXT,
			UNIQUE(account_id, app)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// --- Customers ---

type Customer struct {
	ID   int64
	Name string
}

func (s *Store) AddCustomer(name string) (int64, error) {
	res, err := s.db.Exec("INSERT INTO customers (name) VALUES (?) ON CONFLICT(name) DO NOTHING", name)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if id == 0 {
		// Already existed.
		err = s.db.QueryRow("SELECT id FROM customers WHERE name = ?", name).Scan(&id)
	}
	return id, err
}

func (s *Store) GetCustomer(name string) (*Customer, error) {
	var c Customer
	err := s.db.QueryRow("SELECT id, name FROM customers WHERE name = ?", name).Scan(&c.ID, &c.Name)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &c, err
}

func (s *Store) ListCustomers() ([]Customer, error) {
	rows, err := s.db.Query("SELECT id, name FROM customers ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Customer
	for rows.Next() {
		var c Customer
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- Accounts ---

type Account struct {
	ID         int64
	CustomerID int64
	AccountID  string
	RoleARN    string
	ExternalID string
}

func (s *Store) AddAccount(customerID int64, accountID, roleARN, externalID string) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO accounts (customer_id, account_id, role_arn, external_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(customer_id, account_id) DO UPDATE SET
			role_arn = excluded.role_arn,
			external_id = excluded.external_id
	`, customerID, accountID, roleARN, externalID)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if id == 0 {
		err = s.db.QueryRow("SELECT id FROM accounts WHERE customer_id = ? AND account_id = ?", customerID, accountID).Scan(&id)
	}
	return id, err
}

func (s *Store) GetAccount(customerID int64, accountID string) (*Account, error) {
	var a Account
	err := s.db.QueryRow("SELECT id, customer_id, account_id, role_arn, external_id FROM accounts WHERE customer_id = ? AND account_id = ?", customerID, accountID).
		Scan(&a.ID, &a.CustomerID, &a.AccountID, &a.RoleARN, &a.ExternalID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &a, err
}

// --- Log Groups ---

type LogGroup struct {
	ID           int64
	AccountID    int64
	App          string
	LogGroupName string
	Region       string
	SchemaYAML   string
	PollInterval string
	Enabled      bool
	LastPollAt   string
	LastToken    string

	// Joined fields (populated by EnabledLogGroups).
	CustomerName string
	RoleARN      string
	ExternalID   string
	AWSAccountID string
}

func (s *Store) AddLogGroup(accountID int64, app, logGroup, region, schemaYAML, pollInterval string) error {
	_, err := s.db.Exec(`
		INSERT INTO log_groups (account_id, app, log_group, region, schema_yaml, poll_interval)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, app) DO UPDATE SET
			log_group = excluded.log_group,
			region = excluded.region,
			schema_yaml = excluded.schema_yaml,
			poll_interval = excluded.poll_interval
	`, accountID, app, logGroup, region, schemaYAML, pollInterval)
	return err
}

func (s *Store) RemoveLogGroup(app string) error {
	_, err := s.db.Exec("DELETE FROM log_groups WHERE app = ?", app)
	return err
}

// EnabledLogGroups returns all enabled log groups with joined customer/account info.
func (s *Store) EnabledLogGroups() ([]LogGroup, error) {
	rows, err := s.db.Query(`
		SELECT lg.id, lg.account_id, lg.app, lg.log_group, lg.region, lg.schema_yaml,
		       lg.poll_interval, lg.enabled, COALESCE(lg.last_poll_at, ''), COALESCE(lg.last_token, ''),
		       c.name, a.role_arn, a.external_id, a.account_id
		FROM log_groups lg
		JOIN accounts a ON lg.account_id = a.id
		JOIN customers c ON a.customer_id = c.id
		WHERE lg.enabled = 1
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogGroup
	for rows.Next() {
		var lg LogGroup
		if err := rows.Scan(&lg.ID, &lg.AccountID, &lg.App, &lg.LogGroupName, &lg.Region,
			&lg.SchemaYAML, &lg.PollInterval, &lg.Enabled, &lg.LastPollAt, &lg.LastToken,
			&lg.CustomerName, &lg.RoleARN, &lg.ExternalID, &lg.AWSAccountID); err != nil {
			return nil, err
		}
		out = append(out, lg)
	}
	return out, rows.Err()
}

// AllLogGroups returns all log groups with joined info.
func (s *Store) AllLogGroups() ([]LogGroup, error) {
	rows, err := s.db.Query(`
		SELECT lg.id, lg.account_id, lg.app, lg.log_group, lg.region, lg.schema_yaml,
		       lg.poll_interval, lg.enabled, COALESCE(lg.last_poll_at, ''), COALESCE(lg.last_token, ''),
		       c.name, a.role_arn, a.external_id, a.account_id
		FROM log_groups lg
		JOIN accounts a ON lg.account_id = a.id
		JOIN customers c ON a.customer_id = c.id
		ORDER BY c.name, lg.app
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogGroup
	for rows.Next() {
		var lg LogGroup
		if err := rows.Scan(&lg.ID, &lg.AccountID, &lg.App, &lg.LogGroupName, &lg.Region,
			&lg.SchemaYAML, &lg.PollInterval, &lg.Enabled, &lg.LastPollAt, &lg.LastToken,
			&lg.CustomerName, &lg.RoleARN, &lg.ExternalID, &lg.AWSAccountID); err != nil {
			return nil, err
		}
		out = append(out, lg)
	}
	return out, rows.Err()
}

// UpdatePollState saves the last poll timestamp and pagination token.
func (s *Store) UpdatePollState(app, lastPollAt, lastToken string) error {
	_, err := s.db.Exec("UPDATE log_groups SET last_poll_at = ?, last_token = ? WHERE app = ?", lastPollAt, lastToken, app)
	return err
}

// AppNames returns all distinct app names from enabled log groups.
func (s *Store) AppNames() ([]string, error) {
	rows, err := s.db.Query("SELECT DISTINCT app FROM log_groups WHERE enabled = 1 ORDER BY app")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
