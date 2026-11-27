package logdb

import (
	"os"
	"strings"
)

// ObjectDeleter removes an archived parquet object. path is a single file
// or a hive glob (.../**/*.parquet); the implementation expands a glob to
// its underlying objects. Deleting an already-gone object must be a
// success so the GC sweep is idempotent across retries.
type ObjectDeleter interface {
	DeleteParquet(path string) error
}

// LocalFileDeleter is the dev ObjectDeleter: it removes parquet from the
// local filesystem. A partitioned shard's path is "<dir>/**/*.parquet" —
// the whole directory goes. Missing-file is success (idempotent retry).
type LocalFileDeleter struct{}

func (LocalFileDeleter) DeleteParquet(path string) error {
	if i := strings.Index(path, "/**/"); i >= 0 {
		return ignoreNotExist(os.RemoveAll(path[:i]))
	}
	return ignoreNotExist(os.Remove(path))
}

func ignoreNotExist(err error) error {
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
