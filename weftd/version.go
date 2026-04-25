package weftd

import (
	"context"
	"log"
	"os"
	"strings"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
)

// VersionReader provides the current value of a per-source version
// dimension, sourced from a file on disk that the deploy system
// writes. Values are watched via fsnotify so a deploy that touches
// the file is reflected on the next emitted row, no agent restart
// required. Empty file or read error → empty string returned (the
// dimension is then NULL on the row, queries on $VERSION don't
// filter).
//
// See docs/plans/ingest-dimensions.md for the broader scheme.
type VersionReader struct {
	current atomic.Pointer[string]
}

// NewVersionReader creates a reader, populating the initial value
// from `path`, then watching for changes in the background. If
// `path` is empty, the reader returns "" forever (no version
// dimension stamped). If `path` doesn't exist yet, the reader
// returns "" until the file appears.
//
// The fsnotify watch is registered synchronously before this
// function returns, so callers can safely create or modify the
// file immediately after construction without racing the watcher.
func NewVersionReader(ctx context.Context, path string) *VersionReader {
	r := &VersionReader{}
	empty := ""
	r.current.Store(&empty)

	if path == "" {
		return r
	}

	// Initial read; tolerate missing file.
	r.refreshFrom(path)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("version reader: fsnotify: %v", err)
		return r
	}
	dir, base := splitPath(path)
	if err := w.Add(dir); err != nil {
		log.Printf("version reader: watch %s: %v", dir, err)
		w.Close()
		return r
	}

	go r.runWatch(ctx, w, path, base)
	return r
}

// Current returns the most recent value read from the file. Safe to
// call from any goroutine; never blocks.
func (r *VersionReader) Current() string {
	if v := r.current.Load(); v != nil {
		return *v
	}
	return ""
}

// refreshFrom reads `path` and stores the trimmed contents. Errors
// are logged but never fatal — version is best-effort.
func (r *VersionReader) refreshFrom(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		// Not found / permission denied / etc. — leave the previous
		// value in place rather than blanking it out.
		if !os.IsNotExist(err) {
			log.Printf("version reader: %s: %v", path, err)
		}
		return
	}
	v := strings.TrimSpace(string(data))
	r.current.Store(&v)
}

// runWatch runs the fsnotify event loop. Watches the directory
// containing the target file because editors typically rename-into-
// place rather than truncate-and-rewrite, and a watcher on the file
// directly misses the new inode.
func (r *VersionReader) runWatch(ctx context.Context, w *fsnotify.Watcher, path, base string) {
	defer w.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			// Only respond to changes to the file we care about.
			if filepathBase(ev.Name) != base {
				continue
			}
			// Create | Write | Rename | Remove all warrant a re-read.
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) != 0 {
				r.refreshFrom(path)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("version reader: watch error: %v", err)
		}
	}
}

// splitPath splits a path into (dir, base). Avoids importing
// path/filepath from a hot Linux-only path; weftd already does its
// own minimal path manipulation elsewhere.
func splitPath(p string) (dir, base string) {
	idx := strings.LastIndexByte(p, '/')
	if idx < 0 {
		return ".", p
	}
	if idx == 0 {
		return "/", p[1:]
	}
	return p[:idx], p[idx+1:]
}

func filepathBase(p string) string {
	idx := strings.LastIndexByte(p, '/')
	if idx < 0 {
		return p
	}
	return p[idx+1:]
}
