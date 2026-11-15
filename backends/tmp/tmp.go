// Package tmp is the contract of the /tmp domain: the small scratch files a
// user keeps in the shell, and the Store interface every build implements
// (Postgres for the hosted build, SQLite for the open one; see defaults
// and tmp/tmpstoretest).
//
// It is a file system in miniature. A path is relative to /tmp, slash-separated,
// with no empty, "." or ".." parts; a file's parent directory has to exist, as
// mkdir without -p; a directory is made by Mkdir, or as a parent of a file only
// where a caller says so. Operations are not atomic across each other, as on a
// real file system: two writers to one path race, last one wins.
//
// Whose files they are is said in the build's own coordinates, a shape.Added, as
// in package source: the open build's is the empty value and has one
// implicit owner.
//
// Limits are the caller's: the store enforces the numbers it is handed and knows
// nothing of plans. The hosted build computes them from the owner's tier; the
// open build passes whatever its config says, or zero for none. And the store
// does not expire anything: how long a file lives is the build's business (the
// hosted build lets its storage drop old partitions after 48 hours; the open
// build keeps files until they are removed).
//
// The package imports only the standard library and the shape package, so a
// process that links an implementation stays cgo-free.
package tmp

import (
	"errors"
	"strings"
	"time"

	"github.com/w3ft-net/weft/shape"
)

// Errors an operation returns.
var (
	ErrFileTooLarge  = errors.New("tmp: file exceeds per-file size limit")
	ErrQuotaExceeded = errors.New("tmp: storage quota exceeded")
	ErrIsDir         = errors.New("tmp: is a directory")
	ErrNotDir        = errors.New("tmp: not a directory")
	ErrNotFound      = errors.New("tmp: no such file or directory")
	ErrExists        = errors.New("tmp: already exists")
	ErrNotEmpty      = errors.New("tmp: directory not empty")
	ErrInvalidPath   = errors.New("tmp: invalid path")
)

// Limits are the numbers a write is held to. Zero is no limit.
type Limits struct {
	// MaxFileBytes caps one file's size.
	MaxFileBytes int64
	// QuotaBytes caps the total size of the owner's files. A write that replaces a
	// file counts only its growth, so rewriting at the quota is allowed.
	QuotaBytes int64
}

// File is a file or directory, as listed.
type File struct {
	// Path is relative to /tmp, with no leading or trailing slash.
	Path  string
	IsDir bool
	// Size is the file's length in bytes; 0 for a directory.
	Size int64
	// CreatedBy is who made it, "" if unknown. Editing is not authoring: an
	// overwrite keeps the creator.
	CreatedBy string
	CreatedAt time.Time
	// UpdatedAt is the last write, append, touch or rename.
	UpdatedAt time.Time
}

// Name is the last segment of the path.
func (f File) Name() string {
	if i := strings.LastIndexByte(f.Path, '/'); i >= 0 {
		return f.Path[i+1:]
	}
	return f.Path
}

// Store is the persistence contract of the /tmp domain. Every path argument is
// normalized first (NormalizePath), and a bad one is ErrInvalidPath.
type Store interface {
	// Stat is the file or directory at the path, ErrNotFound if none.
	Stat(owner shape.Added, path string) (*File, error)
	// List is the direct children of a directory, by path; dir "" is the top. A
	// missing directory lists as empty.
	List(owner shape.Added, dir string) ([]File, error)
	// Read is a file's whole contents. ErrIsDir for a directory.
	Read(owner shape.Added, path string) ([]byte, error)

	// Write replaces a file's contents, creating it if need be, and reports
	// whether it created it. The parent directory must exist (ErrNotFound; ErrNotDir
	// if it is a file); the path must not be a directory (ErrIsDir). A new file is
	// credited to by, which may be empty.
	Write(owner shape.Added, lim Limits, by, path string, data []byte) (created bool, err error)
	// Append adds bytes to the end of a file, as O_APPEND does: onto the last
	// byte, whether or not it ended a line. It creates the file if absent, under
	// the same rules as Write.
	Append(owner shape.Added, lim Limits, by, path string, data []byte) (created bool, err error)
	// Touch creates an empty file, or bumps an existing file's or directory's
	// UpdatedAt, reporting whether it created one.
	Touch(owner shape.Added, lim Limits, by, path string) (created bool, err error)
	// Mkdir creates a directory whose parent exists. ErrExists if the path does.
	Mkdir(owner shape.Added, by, path string) error
	// Copy copies a file to a new path; ErrExists if the destination does,
	// ErrIsDir if the source is a directory. The copy is credited to by.
	Copy(owner shape.Added, lim Limits, by, src, dst string) error
	// Rename moves a file or a directory with everything under it. ErrExists if the
	// destination does, ErrInvalidPath if it is inside the source. The creator and
	// content stay; UpdatedAt moves.
	Rename(owner shape.Added, src, dst string) error
	// Remove deletes a file or an empty directory (ErrNotEmpty otherwise).
	Remove(owner shape.Added, path string) error
	// RemoveAll deletes a path and everything under it; ErrNotFound if it is not
	// there.
	RemoveAll(owner shape.Added, path string) error

	// UsageBytes is the total size of the owner's files; directories are free.
	UsageBytes(owner shape.Added) (int64, error)
	// CountByUser is how many files the user created; directories do not count.
	CountByUser(owner shape.Added, by string) (int64, error)
}

// NormalizePath strips the leading and trailing slash of a path and rejects what
// is not one: empty, a NUL, or an empty, "." or ".." part.
func NormalizePath(p string) (string, error) {
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	if p == "" || strings.Contains(p, "\x00") {
		return "", ErrInvalidPath
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return "", ErrInvalidPath
		}
	}
	return p, nil
}

// NormalizeDir is NormalizePath for a directory argument, where "" and "/" mean
// the top.
func NormalizeDir(d string) (string, error) {
	if strings.Trim(d, "/") == "" {
		return "", nil
	}
	return NormalizePath(d)
}

// Parents lists the directories above a normalized path, outermost first:
// "a/b/c" is ["a", "a/b"].
func Parents(p string) []string {
	parts := strings.Split(p, "/")
	var out []string
	for i := 1; i < len(parts); i++ {
		out = append(out, strings.Join(parts[:i], "/"))
	}
	return out
}
