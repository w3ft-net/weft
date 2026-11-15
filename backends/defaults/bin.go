package defaults

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/shape"
)

// BinBackend is a bin.BinBackend over the install's SQLite database.
type BinBackend struct {
	db  *sql.DB
	now func() time.Time
}

var _ bin.Store = (*BinBackend)(nil)

const binSchema = `
CREATE TABLE IF NOT EXISTS bin_scripts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	source TEXT NOT NULL,
	language TEXT NOT NULL DEFAULT 'bash',
	content_hash TEXT NOT NULL,
	message TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL DEFAULT '',
	authored_by TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	deleted_at TEXT
);
`

// NewBinBackend is a store over db, creating its table if need be. db comes from
// sqlitedb.Open and is the caller's to close.
func NewBinBackend(db *sql.DB) (*BinBackend, error) {
	if _, err := db.Exec(binSchema); err != nil {
		return nil, fmt.Errorf("create bin schema: %w", err)
	}
	return &BinBackend{db: db, now: time.Now}, nil
}

// Times are stored as fixed-width UTC text.
const binTimeFmt = "2006-01-02T15:04:05.000000000Z"

func binEnc(t time.Time) string { return t.UTC().Format(binTimeFmt) }

const binCols = `id, name, source, language, content_hash, message, created_by, authored_by, created_at, updated_at`

type binScanner interface{ Scan(dest ...any) error }

func binScan(r binScanner) (*bin.Script, error) {
	var (
		sc               bin.Script
		created, updated string
	)
	if err := r.Scan(&sc.ID, &sc.Name, &sc.Source, &sc.Language, &sc.ContentHash, &sc.Message,
		&sc.CreatedBy, &sc.AuthoredBy, &created, &updated); err != nil {
		return nil, err
	}
	var err error
	if sc.CreatedAt, err = time.Parse(binTimeFmt, created); err != nil {
		return nil, err
	}
	if sc.UpdatedAt, err = time.Parse(binTimeFmt, updated); err != nil {
		return nil, err
	}
	return &sc, nil
}

func (s *BinBackend) inTx(fn func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *BinBackend) Save(_ shape.Added, user, name, source, language string) error {
	hash := bin.ContentHash(source)
	now := binEnc(s.now())
	return s.inTx(func(tx *sql.Tx) error {
		var (
			id      int64
			oldHash string
			deleted sql.NullString
		)
		err := tx.QueryRow(`SELECT id, content_hash, deleted_at FROM bin_scripts WHERE name = ?`, name).Scan(&id, &oldHash, &deleted)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.Exec(`INSERT INTO bin_scripts (name, source, language, content_hash, created_by, authored_by, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, name, source, language, hash, user, user, now, now)
			return err
		}
		if err != nil {
			return err
		}
		if oldHash == hash && !deleted.Valid {
			return nil
		}
		_, err = tx.Exec(`UPDATE bin_scripts SET source = ?, language = ?, content_hash = ?, message = '',
			authored_by = ?, updated_at = ?, deleted_at = NULL WHERE id = ?`, source, language, hash, user, now, id)
		return err
	})
}

func (s *BinBackend) Get(_ shape.Added, name string) (*bin.Script, error) {
	sc, err := binScan(s.db.QueryRow(`SELECT `+binCols+` FROM bin_scripts WHERE name = ? AND deleted_at IS NULL`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, bin.ErrNotFound
	}
	return sc, err
}

func (s *BinBackend) List(shape.Added) ([]bin.Script, error) {
	rows, err := s.db.Query(`SELECT ` + binCols + ` FROM bin_scripts WHERE deleted_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []bin.Script
	for rows.Next() {
		sc, err := binScan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sc)
	}
	return out, rows.Err()
}

func (s *BinBackend) Delete(_ shape.Added, name string) error {
	_, err := s.db.Exec(`UPDATE bin_scripts SET deleted_at = ? WHERE name = ? AND deleted_at IS NULL`, binEnc(s.now()), name)
	return err
}

func (s *BinBackend) AmendMessage(_ shape.Added, authorID, name, message string) error {
	return s.inTx(func(tx *sql.Tx) error {
		var by string
		err := tx.QueryRow(`SELECT authored_by FROM bin_scripts WHERE name = ? AND deleted_at IS NULL`, name).Scan(&by)
		if errors.Is(err, sql.ErrNoRows) {
			return bin.ErrNotFound
		}
		if err != nil {
			return err
		}
		if authorID != "" && by != authorID {
			return bin.ErrNotYours
		}
		_, err = tx.Exec(`UPDATE bin_scripts SET message = ? WHERE name = ? AND deleted_at IS NULL`, message, name)
		return err
	})
}
