package store

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsDuplicate reports whether err is (or wraps) a Postgres
// unique-constraint violation (SQLSTATE 23505). Used by API handlers
// to translate store errors into a clean 409 response instead of
// leaking the raw PgError text (which names the internal constraint).
func IsDuplicate(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
