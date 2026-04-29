package weftd

import "context"

// VersionReader is a thin wrapper around FileValueReader for the
// per-source `$VERSION` dimension: read the whole file, watch for
// changes, return the trimmed contents on each Current() call.
//
// Retained as a named type so call sites that pass *VersionReader
// around stay unchanged. New code should use FileValueReader
// directly through the `dimensions:` config map (see
// docs/plans/dimensions-from-files.md).
type VersionReader struct {
	*FileValueReader
}

// NewVersionReader constructs a VersionReader. An empty path returns
// a reader whose Current() always reports "".
func NewVersionReader(ctx context.Context, path string) *VersionReader {
	return &VersionReader{NewFileValueReader(ctx, FileValueSpec{Path: path})}
}
