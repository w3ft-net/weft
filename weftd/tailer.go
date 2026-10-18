package weftd

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/graybeardcorp/weft/internal/weftlog"
)

const (
	// DefaultMaxLineBytes caps how much of one line is emitted; the rest is
	// consumed (so offsets stay exact) and replaced by a marker.
	DefaultMaxLineBytes = 1 << 20

	fingerprintBytes = 1024
	TruncatedMarker  = "…[line truncated]"
)

// FileID identifies a file independent of its path: device, inode, and a
// hash of its first FPLen bytes. Inode alone is not identity (filesystems
// reuse freed inodes), so the fingerprint disambiguates.
type FileID struct {
	Dev   uint64
	Ino   uint64
	FP    string // hex sha256 of the first FPLen bytes
	FPLen int
}

// Line is one complete line read from a file. End is the byte offset just
// past the line's terminating newline (CR included), i.e. where a resumed
// tailer should start to see the next line.
type Line struct {
	Text string
	End  int64
	File FileID
}

// Tailer watches a log file and emits complete lines on a channel with
// their exact byte offsets. Handles truncation (logrotate copytruncate)
// and rename/recreate rotation, draining the old file before switching.
type Tailer struct {
	path    string
	file    *os.File
	id      FileID
	watcher *fsnotify.Watcher
	lines   chan Line
	done    chan struct{}
	maxLine int

	// Owned by the watch goroutine.
	r       *bufio.Reader
	pos     int64  // bytes consumed from the file, including pending
	pending []byte // partial line awaiting its newline
	skipped int    // bytes of the current line dropped past maxLine
}

// NewTailer starts tailing path from byte offset start. A negative start
// means the current end of file.
func NewTailer(path string, start int64) (*Tailer, error) {
	return newTailer(path, start, DefaultMaxLineBytes)
}

func newTailer(path string, start int64, maxLine int) (*Tailer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	pos, err := seekStart(f, start)
	if err != nil {
		f.Close()
		return nil, err
	}
	id, err := identify(f)
	if err != nil {
		f.Close()
		return nil, err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := watcher.Add(path); err != nil {
		f.Close()
		watcher.Close()
		return nil, err
	}

	t := &Tailer{
		path:    path,
		file:    f,
		id:      id,
		watcher: watcher,
		lines:   make(chan Line, 1024),
		done:    make(chan struct{}),
		maxLine: maxLine,
		r:       bufio.NewReader(f),
		pos:     pos,
	}
	go t.watch()
	return t, nil
}

func seekStart(f *os.File, start int64) (int64, error) {
	if start < 0 {
		return f.Seek(0, io.SeekEnd)
	}
	return f.Seek(start, io.SeekStart)
}

// identify returns the file's device, inode and first-bytes fingerprint.
func identify(f *os.File) (FileID, error) {
	fi, err := f.Stat()
	if err != nil {
		return FileID{}, err
	}
	var id FileID
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		id.Dev, id.Ino = uint64(st.Dev), uint64(st.Ino) //nolint:unconvert // width differs per OS
	}
	id.FP, id.FPLen, err = fingerprintPrefix(f)
	if err != nil {
		return FileID{}, err
	}
	return id, nil
}

// fingerprintPrefix hashes up to the first fingerprintBytes of f and
// reports how many bytes were hashed.
func fingerprintPrefix(f *os.File) (string, int, error) {
	buf := make([]byte, fingerprintBytes)
	n, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return "", 0, err
	}
	sum := sha256.Sum256(buf[:n])
	return hex.EncodeToString(sum[:]), n, nil
}

// fingerprint hashes the first n bytes of f. Resume uses it with the length
// recorded at checkpoint time, so a file that has since grown still matches.
func fingerprint(f *os.File, n int) (string, error) {
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return "", err
	}
	if got < n {
		return "", io.ErrUnexpectedEOF // shorter than when checkpointed: not the same file
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

// Lines returns the channel of new log lines.
func (t *Tailer) Lines() <-chan Line {
	return t.lines
}

// Close stops the tailer.
func (t *Tailer) Close() {
	close(t.done)
	t.watcher.Close()
}

func (t *Tailer) watch() {
	defer close(t.lines)
	defer t.file.Close()

	// Backlog that existed before the first write event (resume from an
	// earlier offset, or a file created before we opened it).
	if !t.drain() {
		return
	}

	for {
		select {
		case <-t.done:
			return

		case event, ok := <-t.watcher.Events:
			if !ok {
				return
			}

			if event.Op&fsnotify.Write == fsnotify.Write {
				t.checkTruncated()
				if !t.drain() {
					return
				}
			}

			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				if !t.rotate() {
					return
				}
			}

		case err, ok := <-t.watcher.Errors:
			if !ok {
				return
			}
			weftlog.Errorf(context.Background(), "tailer[%s]: watch error: %v", t.path, err)
		}
	}
}

// checkTruncated rewinds to 0 when the file has shrunk below our position
// (copytruncate). Anything held in pending belonged to the old contents.
func (t *Tailer) checkTruncated() {
	fi, err := t.file.Stat()
	if err != nil || fi.Size() >= t.pos {
		return
	}
	weftlog.Infof(context.Background(), "tailer[%s]: truncated (size %d < position %d), restarting at 0", t.path, fi.Size(), t.pos)
	if _, err := t.file.Seek(0, io.SeekStart); err != nil {
		return
	}
	t.r.Reset(t.file)
	t.pos, t.pending, t.skipped = 0, nil, 0
	if id, err := identify(t.file); err == nil {
		t.id = id
	}
}

// rotate handles a Rename/Remove event: drain the old fd (still valid
// after a rename) to EOF, flush any final unterminated line, then switch
// to whatever now lives at path. Reports false if the tailer should stop.
func (t *Tailer) rotate() bool {
	time.Sleep(100 * time.Millisecond)

	// Late events for an inode we already left (or a rename of a file we
	// have since replaced) must not re-read the current file from 0.
	if cur, err := os.Stat(t.path); err == nil {
		if mine, err := t.file.Stat(); err == nil && os.SameFile(cur, mine) {
			return true
		}
	}

	if !t.drain() || !t.flushPartial() {
		return false
	}
	f, err := os.Open(t.path)
	if err != nil {
		weftlog.Infof(context.Background(), "tailer[%s]: reopen after rotation: %v", t.path, err)
		return false
	}
	id, err := identify(f)
	if err != nil {
		f.Close()
		return false
	}
	t.file.Close()
	t.file, t.id = f, id
	t.r.Reset(f)
	t.pos, t.pending, t.skipped = 0, nil, 0
	// Re-watch the new file, then pick up anything written before the
	// watch was in place.
	t.watcher.Add(t.path)
	return t.drain()
}

// drain reads every complete line currently available. Reports false if
// the tailer was closed while blocked on the output channel.
func (t *Tailer) drain() bool {
	for {
		chunk, err := t.r.ReadSlice('\n')
		t.pos += int64(len(chunk))
		switch err {
		case nil:
			t.addChunk(chunk)
			if !t.emit(t.takeLine()) {
				return false
			}
		case bufio.ErrBufferFull:
			t.addChunk(chunk)
		default: // io.EOF or a read error: hold the partial line
			t.addChunk(chunk)
			if err != io.EOF {
				weftlog.Infof(context.Background(), "tailer[%s]: read: %v", t.path, err)
			}
			return true
		}
	}
}

// addChunk appends to the pending line, keeping at most maxLine bytes and
// counting the rest as skipped so position accounting stays exact.
func (t *Tailer) addChunk(chunk []byte) {
	room := t.maxLine - len(t.pending)
	if room < 0 {
		room = 0
	}
	if len(chunk) > room {
		t.skipped += len(chunk) - room
		chunk = chunk[:room]
	}
	t.pending = append(t.pending, chunk...)
}

// takeLine converts pending into a Line ending at the current position.
func (t *Tailer) takeLine() Line {
	b := t.pending
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	if n := len(b); n > 0 && b[n-1] == '\r' {
		b = b[:n-1]
	}
	text := string(b)
	if t.skipped > 0 {
		text += TruncatedMarker
	}
	t.pending, t.skipped = t.pending[:0], 0
	t.refreshIdentity()
	return Line{Text: text, End: t.pos, File: t.id}
}

// refreshIdentity re-fingerprints a file that was shorter than
// fingerprintBytes when opened, once it has grown. A checkpoint carrying the
// short prefix hash would match almost any file; the full prefix is what makes
// the identity meaningful. Stops costing anything once the prefix is full.
func (t *Tailer) refreshIdentity() {
	if t.id.FPLen >= fingerprintBytes || t.pos <= int64(t.id.FPLen) {
		return
	}
	if fp, n, err := fingerprintPrefix(t.file); err == nil {
		t.id.FP, t.id.FPLen = fp, n
	}
}

// flushPartial emits an unterminated final line (end of a rotated file).
func (t *Tailer) flushPartial() bool {
	if len(t.pending) == 0 {
		return true
	}
	return t.emit(t.takeLine())
}

func (t *Tailer) emit(l Line) bool {
	select {
	case t.lines <- l:
		return true
	case <-t.done:
		return false
	}
}
