package weftd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/graybeardcorp/weft/internal/weftlog"
)

const (
	checkpointStateFile = "state.json"
	checkpointLockFile  = ".lock"

	// DefaultMaxResumeBytes caps how much backlog a resumed file replays.
	DefaultMaxResumeBytes = 256 << 20
	// checkpointMaxAge prunes entries that have not been updated in a
	// week. Pruning by "path no longer exists" would race rotation.
	checkpointMaxAge = 7 * 24 * time.Hour
)

// ErrCheckpointLocked means another weftd already holds the checkpoint
// directory. Callers run with checkpointing disabled rather than corrupt
// the other process's state.
var ErrCheckpointLocked = errors.New("checkpoint directory is locked by another process")

// CheckpointOptions tunes a CheckpointStore. Zero values pick defaults.
type CheckpointOptions struct {
	MaxResumeBytes int64            // backlog replay cap; <=0 means DefaultMaxResumeBytes
	Now            func() time.Time // test hook
}

// CheckpointCounters tallies the non-trivial resume decisions for one
// source, so gaps stop being silent.
type CheckpointCounters struct {
	Resumed          int64
	NewFile          int64
	RotatedWhileDown int64
	Truncated        int64
	SkippedBytes     int64 // backlog dropped by the max_resume_bytes cap
}

func (c CheckpointCounters) isZero() bool { return c == CheckpointCounters{} }

type cpEntry struct {
	Dev       uint64    `json:"dev"`
	Ino       uint64    `json:"ino"`
	FP        string    `json:"fp"`
	FPLen     int       `json:"fp_len"`
	Offset    int64     `json:"offset"`
	UpdatedAt time.Time `json:"updated_at"`
}

type cpFile struct {
	Version int                `json:"version"`
	Files   map[string]cpEntry `json:"files"`
}

// CheckpointStore persists, per tailed file, the offset up to which lines
// have been delivered, so a restarted weftd resumes instead of seeking to
// EOF. The persisted offset means "delivered", never "read": callers commit
// only after the coordinator acknowledged the lines (see
// docs/plans/weftd-offset-checkpointing.md).
//
// A nil *CheckpointStore is valid and disables checkpointing: Open falls
// back to the pre-checkpoint behaviour (EOF for startup files, 0 for files
// discovered while running) and the returned handle is a no-op.
type CheckpointStore struct {
	dir  string
	opts CheckpointOptions
	lock *os.File

	mu        sync.Mutex
	entries   map[string]cpEntry
	haveState bool // a state file existed (and parsed) when we started
	dirty     bool
	stats     map[string]*CheckpointCounters
}

// OpenCheckpointStore takes the directory lock and loads state.json. A
// corrupt state file is renamed aside and treated as absent; it is never
// fatal, because a fatal-config agent cannot auto-upgrade out of the problem.
func OpenCheckpointStore(dir string, opts CheckpointOptions) (*CheckpointStore, error) {
	if opts.MaxResumeBytes <= 0 {
		opts.MaxResumeBytes = DefaultMaxResumeBytes
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, checkpointLockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrCheckpointLocked
		}
		return nil, err
	}

	c := &CheckpointStore{
		dir:     dir,
		opts:    opts,
		lock:    lock,
		entries: make(map[string]cpEntry),
		stats:   make(map[string]*CheckpointCounters),
	}
	c.load()
	if !c.haveState {
		// Record that checkpointing has run here, so the next start treats a
		// file with no entry as new (read from 0) rather than as a fresh install.
		c.dirty = true
		if err := c.flushLocked(); err != nil {
			lock.Close()
			return nil, err
		}
	}
	return c, nil
}

func (c *CheckpointStore) load() {
	path := filepath.Join(c.dir, checkpointStateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			weftlog.Infof(context.Background(), "checkpoint: read %s: %v — starting without state", path, err)
		}
		return
	}
	var f cpFile
	if err := json.Unmarshal(data, &f); err != nil || f.Version != 1 {
		weftlog.Infof(context.Background(), "checkpoint: %s is unreadable (%v) — moving aside and starting without state", path, err)
		if err := os.Rename(path, path+".corrupt"); err != nil {
			weftlog.Infof(context.Background(), "checkpoint: rename corrupt state: %v", err)
		}
		return
	}
	if f.Files != nil {
		c.entries = f.Files
	}
	c.haveState = true
}

// CheckpointHandle commits delivered offsets for one tailed file. A nil
// handle is a no-op.
type CheckpointHandle struct {
	s   *CheckpointStore
	key string
}

// Open decides where to start tailing path and registers it. startup is true
// for files present when weftd starts (resume rules apply) and false for
// files discovered while running (brand new: start at 0). The returned
// offset is concrete; pass it to NewTailer.
//
// Every tailed file gets an entry immediately, even if nothing is ever
// delivered from it, so a later restart never mistakes a quiet file for a
// new one and replays it from the top.
func (c *CheckpointStore) Open(path string, startup bool) (*CheckpointHandle, int64) {
	fallback := int64(0)
	if startup {
		fallback = -1
	}
	if c == nil {
		return nil, fallback
	}
	key, err := filepath.EvalSymlinks(path)
	if err == nil {
		key, err = filepath.Abs(key)
	}
	if err != nil {
		return nil, fallback
	}
	f, err := os.Open(key)
	if err != nil {
		return nil, fallback
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fallback
	}
	id, err := identify(f)
	if err != nil {
		return nil, fallback
	}
	size := fi.Size()

	c.mu.Lock()
	defer c.mu.Unlock()
	start := c.decideLocked(key, f, id, size, startup)
	c.entries[key] = cpEntry{
		Dev: id.Dev, Ino: id.Ino, FP: id.FP, FPLen: id.FPLen,
		Offset: start, UpdatedAt: c.opts.Now(),
	}
	c.dirty = true
	return &CheckpointHandle{s: c, key: key}, start
}

func (c *CheckpointStore) counters(key string) *CheckpointCounters {
	n := c.stats[key]
	if n == nil {
		n = &CheckpointCounters{}
		c.stats[key] = n
	}
	return n
}

// decideLocked applies the resume table from the plan.
func (c *CheckpointStore) decideLocked(key string, f *os.File, id FileID, size int64, startup bool) int64 {
	if !startup {
		return 0
	}
	if !c.haveState {
		return size // fresh install / checkpointing just enabled: only tail new content
	}
	st := c.counters(key)
	e, ok := c.entries[key]
	if !ok {
		st.NewFile++
		weftlog.Infof(context.Background(), "checkpoint[%s]: new file, reading from start", key)
		return c.capLocked(key, f, 0, size)
	}

	var start int64
	switch {
	case !c.sameFile(f, e, id):
		st.RotatedWhileDown++
		weftlog.Infof(context.Background(), "checkpoint[%s]: rotated while down; previous file's unread tail lost", key)
	case e.Offset > size:
		st.Truncated++
		weftlog.Infof(context.Background(), "checkpoint[%s]: truncated while down, reading from start", key)
	default:
		start = e.Offset
		st.Resumed++
		weftlog.Infof(context.Background(), "checkpoint[%s]: resuming at %d (%d bytes behind)", key, start, size-start)
	}
	return c.capLocked(key, f, start, size)
}

// sameFile reports whether f is the file e was recorded for: device, inode
// and the fingerprint of the same prefix length. Inode alone is not identity.
func (c *CheckpointStore) sameFile(f *os.File, e cpEntry, id FileID) bool {
	if e.Dev != id.Dev || e.Ino != id.Ino {
		return false
	}
	fp, err := fingerprint(f, e.FPLen)
	return err == nil && fp == e.FP
}

// capLocked bounds the backlog replayed from start: beyond MaxResumeBytes,
// skip to the first line boundary inside the allowed window.
func (c *CheckpointStore) capLocked(key string, f *os.File, start, size int64) int64 {
	max := c.opts.MaxResumeBytes
	if size-start <= max {
		return start
	}
	from := size - max
	at, err := nextLineStart(f, from, size)
	if err != nil {
		at = size
	}
	skipped := at - start
	c.counters(key).SkippedBytes += skipped
	weftlog.Infof(context.Background(), "checkpoint[%s]: resume backlog %d bytes exceeds cap %d; skipped %d bytes", key, size-start, max, skipped)
	return at
}

// nextLineStart returns the offset of the first line beginning at or after
// from, or size if there is none.
func nextLineStart(f *os.File, from, size int64) (int64, error) {
	if from <= 0 {
		return 0, nil
	}
	buf := make([]byte, 64<<10)
	pos := from - 1 // include the previous byte: if it is '\n', from is already a line start
	for pos < size {
		n, err := f.ReadAt(buf, pos)
		if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
			return pos + int64(i) + 1, nil
		}
		pos += int64(n)
		if err != nil {
			if err == io.EOF {
				break
			}
			return 0, err
		}
	}
	return size, nil
}

// Commit records that every line of the file identified by id up to
// offset has been delivered. Safe to call from many goroutines.
func (h *CheckpointHandle) Commit(id FileID, offset int64) {
	if h == nil {
		return
	}
	h.s.mu.Lock()
	h.s.entries[h.key] = cpEntry{
		Dev: id.Dev, Ino: id.Ino, FP: id.FP, FPLen: id.FPLen,
		Offset: offset, UpdatedAt: h.s.opts.Now(),
	}
	h.s.dirty = true
	h.s.mu.Unlock()
}

// Stats returns and resets the per-source counters, omitting sources with
// nothing to report.
func (c *CheckpointStore) Stats() map[string]CheckpointCounters {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]CheckpointCounters)
	for k, v := range c.stats {
		if !v.isZero() {
			out[k] = *v
		}
		*v = CheckpointCounters{}
	}
	return out
}

// Flush atomically writes state.json if anything changed: temp file, fsync,
// rename, fsync the directory.
func (c *CheckpointStore) Flush() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.flushLocked()
}

func (c *CheckpointStore) flushLocked() error {
	if !c.dirty {
		return nil
	}
	cutoff := c.opts.Now().Add(-checkpointMaxAge)
	for k, e := range c.entries {
		if e.UpdatedAt.Before(cutoff) {
			delete(c.entries, k)
		}
	}
	data, err := json.Marshal(cpFile{Version: 1, Files: c.entries})
	if err != nil {
		return err
	}
	final := filepath.Join(c.dir, checkpointStateFile)
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp)
		return fmt.Errorf("write checkpoint: %w", werr)
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	if d, err := os.Open(c.dir); err == nil {
		d.Sync()
		d.Close()
	}
	c.dirty = false
	return nil
}

// Run flushes every interval until ctx is done. It does not flush on exit:
// the owner calls Close after the shippers have made their final commits.
func (c *CheckpointStore) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.Flush(); err != nil {
				weftlog.Infof(ctx, "checkpoint: flush: %v", err)
			}
		}
	}
}

// Close performs the final flush and releases the directory lock.
func (c *CheckpointStore) Close() error {
	if c == nil {
		return nil
	}
	err := c.Flush()
	c.lock.Close() // releases the flock
	return err
}
