package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/w3ft-net/weft/backends/token"
	"github.com/w3ft-net/weft/shape"
)

// TokenBackend is a token.TokenBackend over the install's SQLite database.
type TokenBackend struct {
	db  *sql.DB
	now func() time.Time
}

var _ token.Store = (*TokenBackend)(nil)

const tokenSchema = `
CREATE TABLE IF NOT EXISTS tokens (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	token_hash TEXT NOT NULL UNIQUE,
	public_id TEXT NOT NULL UNIQUE,
	prefix TEXT NOT NULL,
	namespace_id INTEGER NOT NULL REFERENCES namespaces (id) ON DELETE CASCADE,
	label TEXT NOT NULL DEFAULT '',
	env TEXT NOT NULL DEFAULT '',
	revoked INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	metrics_enabled INTEGER NOT NULL DEFAULT 0,
	metrics_interval TEXT NOT NULL DEFAULT '',
	metrics_collectors TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS tokens_namespace ON tokens (namespace_id);
`

// NewTokenBackend is a store over db, creating its table if need be. db comes from
// sqlitedb.Open and is the caller's to close. The namespaces table must already
// exist: create the source store on the same db first.
func NewTokenBackend(db *sql.DB) (*TokenBackend, error) {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'namespaces'`).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errors.New("create tokens store: no namespaces table; create the sources store on this database first")
	}
	if _, err := db.Exec(tokenSchema); err != nil {
		return nil, fmt.Errorf("create tokens schema: %w", err)
	}
	return &TokenBackend{db: db, now: time.Now}, nil
}

const tokenTimeFmt = "2006-01-02T15:04:05.000000000Z"

func (s *TokenBackend) CreateToken(_ shape.Added, namespaceID uint, label, env string) (string, *token.Token, error) {
	raw, err := token.NewRaw()
	if err != nil {
		return "", nil, err
	}
	publicID, err := token.NewPublicID()
	if err != nil {
		return "", nil, err
	}
	var name string
	err = s.db.QueryRow(`SELECT name FROM namespaces WHERE id = ?`, namespaceID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, fmt.Errorf("namespace %d does not exist", namespaceID)
	}
	if err != nil {
		return "", nil, err
	}
	created := s.now().UTC()
	res, err := s.db.Exec(`INSERT INTO tokens (token_hash, public_id, prefix, namespace_id, label, env, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		token.Hash(raw), publicID, token.Prefix(raw), namespaceID, label, env, created.Format(tokenTimeFmt))
	if err != nil {
		return "", nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", nil, err
	}
	return raw, &token.Token{
		ID: uint(id), PublicID: publicID, Prefix: token.Prefix(raw),
		Namespace: token.NamespaceRef{ID: namespaceID, Name: name},
		Label:     label, Env: env, CreatedAt: created,
	}, nil
}

func (s *TokenBackend) ValidateToken(raw string) (*token.Auth, error) {
	var a token.Auth
	err := s.db.QueryRow(`SELECT t.id, n.name, t.env FROM tokens t JOIN namespaces n ON n.id = t.namespace_id
		WHERE t.token_hash = ? AND t.revoked = 0`, token.Hash(raw)).
		Scan(&a.TokenID, &a.Namespace, &a.Env)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *TokenBackend) RevokeToken(_ shape.Added, publicID string) error {
	res, err := s.db.Exec(`UPDATE tokens SET revoked = 1 WHERE public_id = ?`, publicID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return token.ErrNotFound
	}
	return nil
}

func (s *TokenBackend) ListTokens(shape.Added) ([]token.Token, error) {
	rows, err := s.db.Query(`SELECT t.id, t.public_id, t.prefix, t.namespace_id, n.name, t.label, t.env, t.revoked, t.created_at,
		t.metrics_enabled, t.metrics_interval, t.metrics_collectors
		FROM tokens t JOIN namespaces n ON n.id = t.namespace_id ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []token.Token
	for rows.Next() {
		var (
			t                       token.Token
			revoked, metricsEnabled int
			created, collectors     string
		)
		if err := rows.Scan(&t.ID, &t.PublicID, &t.Prefix, &t.Namespace.ID, &t.Namespace.Name, &t.Label, &t.Env, &revoked,
			&created, &metricsEnabled, &t.MetricsInterval, &collectors); err != nil {
			return nil, err
		}
		t.Revoked, t.MetricsEnabled = revoked != 0, metricsEnabled != 0
		if t.CreatedAt, err = time.Parse(tokenTimeFmt, created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(collectors), &t.MetricsCollectors); err != nil {
			return nil, fmt.Errorf("token %s collectors: %w", t.PublicID, err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *TokenBackend) CountLiveTokens(_ shape.Added, namespaceID uint) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM tokens WHERE namespace_id = ? AND revoked = 0`, namespaceID).Scan(&n)
	return n, err
}

func (s *TokenBackend) SetMetricsConfig(raw string, cfg token.MetricsConfig) error {
	collectors := "[]"
	if len(cfg.Collectors) > 0 {
		b, _ := json.Marshal(cfg.Collectors)
		collectors = string(b)
	}
	enabled := 0
	if cfg.Enabled {
		enabled = 1
	}
	_, err := s.db.Exec(`UPDATE tokens SET metrics_enabled = ?, metrics_interval = ?, metrics_collectors = ? WHERE token_hash = ?`,
		enabled, cfg.Interval, collectors, token.Hash(raw))
	return err
}
