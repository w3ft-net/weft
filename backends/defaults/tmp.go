package defaults

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/w3ft-net/weft/backends/tmp"
	"github.com/w3ft-net/weft/shape"
)

// TmpBackend is a tmp.TmpBackend over the install's SQLite database.
type TmpBackend struct {
	db  *sql.DB
	now func() time.Time
}

var _ tmp.Store = (*TmpBackend)(nil)

const tmpSchema = `
CREATE TABLE IF NOT EXISTS tmp_files (
	path TEXT PRIMARY KEY,
	is_dir INTEGER NOT NULL DEFAULT 0,
	content BLOB NOT NULL DEFAULT x'',
	byte_size INTEGER NOT NULL DEFAULT 0,
	created_by TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
) WITHOUT ROWID;
`

// NewTmpBackend is a store over db, creating its table if need be. db comes from
// sqlitedb.Open and is the caller's to close.
func NewTmpBackend(db *sql.DB) (*TmpBackend, error) {
	if _, err := db.Exec(tmpSchema); err != nil {
		return nil, fmt.Errorf("create tmp schema: %w", err)
	}
	return &TmpBackend{db: db, now: time.Now}, nil
}

// Times are stored as fixed-width UTC text.
const tmpTimeFmt = "2006-01-02T15:04:05.000000000Z"

func tmpEnc(t time.Time) string { return t.UTC().Format(tmpTimeFmt) }

// tmpQueryer is what a *sql.DB and a *sql.Tx share.
type tmpQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

func (s *TmpBackend) inTx(fn func(tx *sql.Tx) error) error {
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

const tmpFileCols = `path, is_dir, byte_size, created_by, created_at, updated_at`

type tmpScanner interface{ Scan(dest ...any) error }

func tmpScanFile(r tmpScanner) (*tmp.File, error) {
	var (
		f                tmp.File
		isDir            int
		created, updated string
	)
	if err := r.Scan(&f.Path, &isDir, &f.Size, &f.CreatedBy, &created, &updated); err != nil {
		return nil, err
	}
	f.IsDir = isDir != 0
	var err error
	if f.CreatedAt, err = time.Parse(tmpTimeFmt, created); err != nil {
		return nil, err
	}
	if f.UpdatedAt, err = time.Parse(tmpTimeFmt, updated); err != nil {
		return nil, err
	}
	return &f, nil
}

func tmpGet(q tmpQueryer, path string) (*tmp.File, error) {
	f, err := tmpScanFile(q.QueryRow(`SELECT `+tmpFileCols+` FROM tmp_files WHERE path = ?`, path))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tmp.ErrNotFound
	}
	return f, err
}

// tmpUnder is the SQL condition and arguments for the paths strictly below dir: a
// range scan, so a name's % and _ are only ever themselves.
func tmpUnder(dir string) (string, []any) {
	return `path > ? AND path < ?`, []any{dir + "/", dir + "0"} // '0' is the byte after '/'
}

// tmpEnsureParents checks that every directory above path exists.
func tmpEnsureParents(q tmpQueryer, path string) error {
	for _, p := range tmp.Parents(path) {
		d, err := tmpGet(q, p)
		if errors.Is(err, tmp.ErrNotFound) {
			return fmt.Errorf("%w: parent %q does not exist", tmp.ErrNotFound, p)
		}
		if err != nil {
			return err
		}
		if !d.IsDir {
			return fmt.Errorf("%w: %q is not a directory", tmp.ErrNotDir, p)
		}
	}
	return nil
}

func tmpUsage(q tmpQueryer) (int64, error) {
	var n int64
	err := q.QueryRow(`SELECT COALESCE(SUM(byte_size), 0) FROM tmp_files WHERE is_dir = 0`).Scan(&n)
	return n, err
}

func (s *TmpBackend) Stat(_ shape.Added, path string) (*tmp.File, error) {
	p, err := tmp.NormalizePath(path)
	if err != nil {
		return nil, err
	}
	return tmpGet(s.db, p)
}

func (s *TmpBackend) List(_ shape.Added, dir string) ([]tmp.File, error) {
	d, err := tmp.NormalizeDir(dir)
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + tmpFileCols + ` FROM tmp_files WHERE `
	var args []any
	if d == "" {
		q += `instr(path, '/') = 0`
	} else {
		cond, a := tmpUnder(d)
		// Direct children only: no further slash after the directory's own prefix.
		q += cond + ` AND instr(substr(path, ?), '/') = 0`
		args = append(a, len(d)+2)
	}
	rows, err := s.db.Query(q+` ORDER BY path`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tmp.File
	for rows.Next() {
		f, err := tmpScanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func (s *TmpBackend) Read(_ shape.Added, path string) ([]byte, error) {
	p, err := tmp.NormalizePath(path)
	if err != nil {
		return nil, err
	}
	var (
		isDir   int
		content []byte
	)
	err = s.db.QueryRow(`SELECT is_dir, content FROM tmp_files WHERE path = ?`, p).Scan(&isDir, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, tmp.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if isDir != 0 {
		return nil, tmp.ErrIsDir
	}
	if content == nil {
		content = []byte{}
	}
	return content, nil
}

// tmpCheck refuses a file of size over the per-file limit, or growth by delta past
// the quota given what the owner already uses.
func tmpCheck(q tmpQueryer, lim tmp.Limits, size, delta int64) error {
	if lim.MaxFileBytes > 0 && size > lim.MaxFileBytes {
		return fmt.Errorf("%w (%d bytes > %d bytes)", tmp.ErrFileTooLarge, size, lim.MaxFileBytes)
	}
	if lim.QuotaBytes > 0 && delta > 0 {
		used, err := tmpUsage(q)
		if err != nil {
			return err
		}
		if used+delta > lim.QuotaBytes {
			return fmt.Errorf("%w (%d + %d > %d)", tmp.ErrQuotaExceeded, used, delta, lim.QuotaBytes)
		}
	}
	return nil
}

// put writes data as the contents of path: the whole file, or appended to the
// end of what is there.
func (s *TmpBackend) put(lim tmp.Limits, by, path string, data []byte, appendTo bool) (created bool, err error) {
	p, err := tmp.NormalizePath(path)
	if err != nil {
		return false, err
	}
	if data == nil {
		data = []byte{} // a nil slice binds as NULL
	}
	err = s.inTx(func(tx *sql.Tx) error {
		if err := tmpEnsureParents(tx, p); err != nil {
			return err
		}
		existing, err := tmpGet(tx, p)
		switch {
		case errors.Is(err, tmp.ErrNotFound):
			existing = nil
		case err != nil:
			return err
		case existing.IsDir:
			return tmp.ErrIsDir
		}
		var oldSize int64
		if existing != nil {
			oldSize = existing.Size
		}
		newSize, delta := int64(len(data)), int64(len(data))-oldSize
		if appendTo {
			newSize, delta = oldSize+int64(len(data)), int64(len(data))
		}
		if err := tmpCheck(tx, lim, newSize, delta); err != nil {
			return err
		}
		now := tmpEnc(s.now())
		if existing == nil {
			created = true
			_, err = tx.Exec(`INSERT INTO tmp_files (path, is_dir, content, byte_size, created_by, created_at, updated_at)
				VALUES (?, 0, ?, ?, ?, ?, ?)`, p, data, newSize, by, now, now)
			return err
		}
		if appendTo {
			_, err = tx.Exec(`UPDATE tmp_files SET content = content || ?, byte_size = ?, updated_at = ? WHERE path = ?`, data, newSize, now, p)
		} else {
			_, err = tx.Exec(`UPDATE tmp_files SET content = ?, byte_size = ?, updated_at = ? WHERE path = ?`, data, newSize, now, p)
		}
		return err
	})
	return created, err
}

func (s *TmpBackend) Write(_ shape.Added, lim tmp.Limits, by, path string, data []byte) (bool, error) {
	return s.put(lim, by, path, data, false)
}

func (s *TmpBackend) Append(_ shape.Added, lim tmp.Limits, by, path string, data []byte) (bool, error) {
	return s.put(lim, by, path, data, true)
}

func (s *TmpBackend) Touch(_ shape.Added, lim tmp.Limits, by, path string) (bool, error) {
	p, err := tmp.NormalizePath(path)
	if err != nil {
		return false, err
	}
	var created bool
	err = s.inTx(func(tx *sql.Tx) error {
		if _, err := tmpGet(tx, p); errors.Is(err, tmp.ErrNotFound) {
			if err := tmpEnsureParents(tx, p); err != nil {
				return err
			}
			created = true
			now := tmpEnc(s.now())
			_, err = tx.Exec(`INSERT INTO tmp_files (path, is_dir, content, byte_size, created_by, created_at, updated_at)
				VALUES (?, 0, x'', 0, ?, ?, ?)`, p, by, now, now)
			return err
		} else if err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE tmp_files SET updated_at = ? WHERE path = ?`, tmpEnc(s.now()), p)
		return err
	})
	return created, err
}

func (s *TmpBackend) Mkdir(_ shape.Added, by, path string) error {
	p, err := tmp.NormalizePath(path)
	if err != nil {
		return err
	}
	return s.inTx(func(tx *sql.Tx) error {
		if err := tmpEnsureParents(tx, p); err != nil {
			return err
		}
		if _, err := tmpGet(tx, p); err == nil {
			return tmp.ErrExists
		} else if !errors.Is(err, tmp.ErrNotFound) {
			return err
		}
		now := tmpEnc(s.now())
		_, err := tx.Exec(`INSERT INTO tmp_files (path, is_dir, created_by, created_at, updated_at) VALUES (?, 1, ?, ?, ?)`, p, by, now, now)
		return err
	})
}

func (s *TmpBackend) Copy(_ shape.Added, lim tmp.Limits, by, src, dst string) error {
	from, err := tmp.NormalizePath(src)
	if err != nil {
		return err
	}
	to, err := tmp.NormalizePath(dst)
	if err != nil {
		return err
	}
	return s.inTx(func(tx *sql.Tx) error {
		f, err := tmpGet(tx, from)
		if err != nil {
			return err
		}
		if f.IsDir {
			return fmt.Errorf("%w: cannot copy a directory", tmp.ErrIsDir)
		}
		if _, err := tmpGet(tx, to); err == nil {
			return tmp.ErrExists
		} else if !errors.Is(err, tmp.ErrNotFound) {
			return err
		}
		if err := tmpEnsureParents(tx, to); err != nil {
			return err
		}
		if err := tmpCheck(tx, lim, f.Size, f.Size); err != nil {
			return err
		}
		now := tmpEnc(s.now())
		_, err = tx.Exec(`INSERT INTO tmp_files (path, is_dir, content, byte_size, created_by, created_at, updated_at)
			SELECT ?, 0, content, byte_size, ?, ?, ? FROM tmp_files WHERE path = ?`, to, by, now, now, from)
		return err
	})
}

func (s *TmpBackend) Rename(_ shape.Added, src, dst string) error {
	from, err := tmp.NormalizePath(src)
	if err != nil {
		return err
	}
	to, err := tmp.NormalizePath(dst)
	if err != nil {
		return err
	}
	if strings.HasPrefix(to, from+"/") {
		return fmt.Errorf("%w: cannot move %q into itself", tmp.ErrInvalidPath, from)
	}
	return s.inTx(func(tx *sql.Tx) error {
		f, err := tmpGet(tx, from)
		if err != nil {
			return err
		}
		if _, err := tmpGet(tx, to); err == nil {
			return tmp.ErrExists
		} else if !errors.Is(err, tmp.ErrNotFound) {
			return err
		}
		if err := tmpEnsureParents(tx, to); err != nil {
			return err
		}
		now := tmpEnc(s.now())
		if _, err := tx.Exec(`UPDATE tmp_files SET path = ?, updated_at = ? WHERE path = ?`, to, now, from); err != nil {
			return err
		}
		if !f.IsDir {
			return nil
		}
		// Everything under a directory moves with it, keeping its own times.
		cond, args := tmpUnder(from)
		_, err = tx.Exec(`UPDATE tmp_files SET path = ? || substr(path, ?) WHERE `+cond, append([]any{to, len(from) + 1}, args...)...)
		return err
	})
}

func (s *TmpBackend) Remove(_ shape.Added, path string) error {
	p, err := tmp.NormalizePath(path)
	if err != nil {
		return err
	}
	return s.inTx(func(tx *sql.Tx) error {
		f, err := tmpGet(tx, p)
		if err != nil {
			return err
		}
		if f.IsDir {
			cond, args := tmpUnder(p)
			var n int
			if err := tx.QueryRow(`SELECT count(*) FROM tmp_files WHERE `+cond, args...).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return fmt.Errorf("%w: %s", tmp.ErrNotEmpty, p)
			}
		}
		_, err = tx.Exec(`DELETE FROM tmp_files WHERE path = ?`, p)
		return err
	})
}

func (s *TmpBackend) RemoveAll(_ shape.Added, path string) error {
	p, err := tmp.NormalizePath(path)
	if err != nil {
		return err
	}
	return s.inTx(func(tx *sql.Tx) error {
		cond, args := tmpUnder(p)
		res, err := tx.Exec(`DELETE FROM tmp_files WHERE path = ? OR (`+cond+`)`, append([]any{p}, args...)...)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return tmp.ErrNotFound
		}
		return nil
	})
}

func (s *TmpBackend) UsageBytes(shape.Added) (int64, error) { return tmpUsage(s.db) }

func (s *TmpBackend) CountByUser(_ shape.Added, by string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT count(*) FROM tmp_files WHERE is_dir = 0 AND created_by = ?`, by).Scan(&n)
	return n, err
}
