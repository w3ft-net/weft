// Package parquet is the open build's archive format: Parquet files in hive
// layouts, split by time or by log key, as the DuckDB store writes them and the
// DuckDB reader reads them. Its layouts, widths and paths are in layout.go, and
// its completion record in manifest.go. Nothing here depends on the engine that
// writes or reads the files.
package parquet

import (
	"strings"

	"github.com/w3ft-net/weft/backends/logs/archive"
)

// The IDs of the formats, which are how an archive's layout is told apart.
const (
	// FormatFile is one Parquet file.
	FormatFile = "parquet-file-v1"
	// FormatTime is hive-partitioned by time bucket: m<minutes>, or no width
	// segment for archives made before the width was recorded.
	FormatTime = "parquet-time-v1"
	// FormatKeyed is hive-partitioned by log key with one bucket width for every
	// key (k<minutes>). Still read; no longer written.
	FormatKeyed = "parquet-keyed-v1"
	// FormatSized is hive-partitioned by log key, each key with a bucket width
	// of its own.
	FormatSized = "parquet-sized-v1"
)

type format struct {
	id         string
	recognizes func(path string) bool
}

func (f format) ID() string                  { return f.id }
func (f format) Recognizes(path string) bool { return f.recognizes(path) }

// Formats are the formats of the archives this build writes and reads, for a
// build's main to register (archive.Registry.AddFormat). Each recognizes a path
// no other does, which is how an archive with no recorded ID says which it is:
// the layout is in the path suffix and nowhere else.
func Formats() []archive.Format {
	return []archive.Format{
		format{FormatFile, func(p string) bool {
			return strings.HasSuffix(p, ".parquet") && !strings.Contains(p, "*")
		}},
		format{FormatTime, func(p string) bool {
			return strings.HasSuffix(p, "/**/*.parquet") && !IsKeyed(p)
		}},
		format{FormatKeyed, isKeyedWidth},
		format{FormatSized, IsSized},
	}
}
