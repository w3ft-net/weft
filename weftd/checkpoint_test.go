package weftd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Resume-table tests for docs/plans/weftd-offset-checkpointing.md.

func writeLog(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func openStore(t *testing.T, dir string, opts CheckpointOptions) *CheckpointStore {
	t.Helper()
	c, err := OpenCheckpointStore(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fileID reads the identity of the file at path the way the tailer does.
func fileID(t *testing.T, path string) FileID {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	id, err := identify(f)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// commitThenRestart opens path as a startup file, commits offset, flushes,
// closes the store and returns a fresh store over the same directory.
func commitThenRestart(t *testing.T, dir, path string, offset int64, opts CheckpointOptions) *CheckpointStore {
	t.Helper()
	c := openStore(t, dir, opts)
	h, _ := c.Open(path, true)
	h.Commit(fileID(t, path), offset)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return openStore(t, dir, opts)
}

func TestCheckpoint_FreshInstallStartsAtEOF(t *testing.T) {
	dir, log := t.TempDir(), filepath.Join(t.TempDir(), "a.log")
	writeLog(t, log, "one\ntwo\n")
	c := openStore(t, dir, CheckpointOptions{})
	defer c.Close()

	_, start := c.Open(log, true)
	if start != 8 {
		t.Errorf("fresh install start = %d, want EOF (8)", start)
	}
}

func TestCheckpoint_ResumesAtCommittedOffset(t *testing.T) {
	dir, log := t.TempDir(), filepath.Join(t.TempDir(), "a.log")
	writeLog(t, log, "one\ntwo\nthree\n")
	c := commitThenRestart(t, dir, log, 4, CheckpointOptions{})
	defer c.Close()

	_, start := c.Open(log, true)
	if start != 4 {
		t.Errorf("start = %d, want committed 4", start)
	}
	if got := c.Stats()[mustKey(t, log)]; got.Resumed != 1 {
		t.Errorf("stats = %+v, want Resumed=1", got)
	}
}

func TestCheckpoint_FileAppendedWhileDownStillMatches(t *testing.T) {
	dir, log := t.TempDir(), filepath.Join(t.TempDir(), "a.log")
	writeLog(t, log, "one\n") // shorter than the fingerprint window
	c := commitThenRestart(t, dir, log, 4, CheckpointOptions{})
	defer c.Close()

	writeLog(t, log, "one\ntwo\nthree\n")
	if _, start := c.Open(log, true); start != 4 {
		t.Errorf("grown file start = %d, want 4", start)
	}
}

func TestCheckpoint_NewFileWhileDownStartsAtZero(t *testing.T) {
	dir, tmp := t.TempDir(), t.TempDir()
	seen, fresh := filepath.Join(tmp, "seen.log"), filepath.Join(tmp, "fresh.log")
	writeLog(t, seen, "x\n")
	c := commitThenRestart(t, dir, seen, 2, CheckpointOptions{})
	defer c.Close()

	writeLog(t, fresh, "a\nb\n")
	if _, start := c.Open(fresh, true); start != 0 {
		t.Errorf("file with no entry start = %d, want 0", start)
	}
	if got := c.Stats()[mustKey(t, fresh)]; got.NewFile != 1 {
		t.Errorf("stats = %+v, want NewFile=1", got)
	}
}

func TestCheckpoint_QuietFileIsNotReplayedAfterRestart(t *testing.T) {
	// A file opened at EOF but never written to must still get an entry,
	// or the next restart would treat it as new and replay it from 0.
	dir, log := t.TempDir(), filepath.Join(t.TempDir(), "quiet.log")
	writeLog(t, log, "old history\nmore history\n")
	c := openStore(t, dir, CheckpointOptions{})
	_, start := c.Open(log, true)
	if start != 25 {
		t.Fatalf("first start = %d, want EOF", start)
	}
	c.Close()

	c = openStore(t, dir, CheckpointOptions{})
	defer c.Close()
	if _, start := c.Open(log, true); start != 25 {
		t.Errorf("restart start = %d, want 25 (history replayed)", start)
	}
}

func TestCheckpoint_TruncatedWhileDown(t *testing.T) {
	dir, log := t.TempDir(), filepath.Join(t.TempDir(), "a.log")
	// Long enough that the fingerprint window (1024) is full.
	body := strings.Repeat("0123456789abcdef\n", 120) // 2040 bytes
	writeLog(t, log, body)
	c := commitThenRestart(t, dir, log, int64(len(body)), CheckpointOptions{})
	defer c.Close()

	// Same inode, same first 1024 bytes, but shorter than the committed offset.
	if err := os.Truncate(log, 1100); err != nil {
		t.Fatal(err)
	}
	if _, start := c.Open(log, true); start != 0 {
		t.Errorf("start = %d, want 0", start)
	}
	if got := c.Stats()[mustKey(t, log)]; got.Truncated != 1 {
		t.Errorf("stats = %+v, want Truncated=1", got)
	}
}

func TestCheckpoint_RotatedWhileDownStartsAtZero(t *testing.T) {
	dir, tmp := t.TempDir(), t.TempDir()
	log := filepath.Join(tmp, "a.log")
	writeLog(t, log, "original\n")
	c := commitThenRestart(t, dir, log, 9, CheckpointOptions{})
	defer c.Close()

	if err := os.Rename(log, log+".1"); err != nil {
		t.Fatal(err)
	}
	writeLog(t, log, "brand new file\nwith lines\n")
	_, start := c.Open(log, true)
	if start != 0 {
		t.Errorf("start = %d, want 0 (file replaced while down)", start)
	}
	if got := c.Stats()[mustKey(t, log)]; got.RotatedWhileDown != 1 {
		t.Errorf("stats = %+v, want RotatedWhileDown=1", got)
	}
}

func TestCheckpoint_SameInodeDifferentFingerprintIsNotSameFile(t *testing.T) {
	// Overwrite in place (same inode) with different content of at least the
	// original length: inode matches, fingerprint must not.
	dir, log := t.TempDir(), filepath.Join(t.TempDir(), "a.log")
	writeLog(t, log, "AAAAAAAAAA\n")
	c := commitThenRestart(t, dir, log, 11, CheckpointOptions{})
	defer c.Close()

	f, _ := os.OpenFile(log, os.O_WRONLY|os.O_TRUNC, 0o644)
	f.WriteString("BBBBBBBBBB\nplus more\n")
	f.Close()
	if _, start := c.Open(log, true); start != 0 {
		t.Errorf("start = %d, want 0 (same inode, different content)", start)
	}
}

func TestCheckpoint_BacklogCapSkipsToLineBoundary(t *testing.T) {
	dir, log := t.TempDir(), filepath.Join(t.TempDir(), "a.log")
	writeLog(t, log, "line-0001\n")
	c := commitThenRestart(t, dir, log, 10, CheckpointOptions{MaxResumeBytes: 50})
	defer c.Close()

	var sb strings.Builder
	sb.WriteString("line-0001\n")
	for i := 2; i <= 30; i++ { // 29 more 10-byte lines
		sb.WriteString("line-" + pad4(i) + "\n")
	}
	writeLog(t, log, sb.String())

	_, start := c.Open(log, true)
	size := int64(sb.Len())
	if size-start > 50 {
		t.Errorf("start %d leaves %d bytes, cap is 50", start, size-start)
	}
	if start%10 != 0 {
		t.Errorf("start %d is not on a line boundary", start)
	}
	if got := c.Stats()[mustKey(t, log)]; got.SkippedBytes != start-10 {
		t.Errorf("SkippedBytes = %d, want %d", got.SkippedBytes, start-10)
	}
}

func pad4(i int) string {
	s := "0000" + string(rune('0'+i/10)) + string(rune('0'+i%10))
	return s[len(s)-4:]
}

func TestCheckpoint_NewFileDiscoveredWhileRunningStartsAtZero(t *testing.T) {
	dir, log := t.TempDir(), filepath.Join(t.TempDir(), "a.log")
	writeLog(t, log, "already here\n")
	c := openStore(t, dir, CheckpointOptions{})
	defer c.Close()

	if _, start := c.Open(log, false); start != 0 {
		t.Errorf("runtime-discovered file start = %d, want 0", start)
	}
}

func TestCheckpoint_SymlinkResolvesToTarget(t *testing.T) {
	dir, tmp := t.TempDir(), t.TempDir()
	target, link := filepath.Join(tmp, "pod.log"), filepath.Join(tmp, "container.log")
	writeLog(t, target, "abc\ndef\n")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	c := openStore(t, dir, CheckpointOptions{})
	h, _ := c.Open(link, true)
	h.Commit(fileID(t, target), 4)
	c.Close()

	c = openStore(t, dir, CheckpointOptions{})
	defer c.Close()
	if _, start := c.Open(link, true); start != 4 {
		t.Errorf("start via symlink = %d, want 4", start)
	}
}

func TestCheckpoint_CorruptStateIsMovedAsideNotFatal(t *testing.T) {
	dir, log := t.TempDir(), filepath.Join(t.TempDir(), "a.log")
	writeLog(t, log, "one\ntwo\n")
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := OpenCheckpointStore(dir, CheckpointOptions{})
	if err != nil {
		t.Fatalf("corrupt state must not be fatal: %v", err)
	}
	defer c.Close()

	if _, err := os.Stat(filepath.Join(dir, "state.json.corrupt")); err != nil {
		t.Errorf("corrupt file not preserved: %v", err)
	}
	if _, start := c.Open(log, true); start != 8 {
		t.Errorf("start = %d, want EOF (8) after discarding corrupt state", start)
	}
}

func TestCheckpoint_HeldLockIsReported(t *testing.T) {
	dir := t.TempDir()
	first := openStore(t, dir, CheckpointOptions{})
	defer first.Close()

	_, err := OpenCheckpointStore(dir, CheckpointOptions{})
	if !errors.Is(err, ErrCheckpointLocked) {
		t.Errorf("second open err = %v, want ErrCheckpointLocked", err)
	}
}

func TestCheckpoint_LockReleasedOnClose(t *testing.T) {
	dir := t.TempDir()
	openStore(t, dir, CheckpointOptions{}).Close()
	c, err := OpenCheckpointStore(dir, CheckpointOptions{})
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	c.Close()
}

func TestCheckpoint_PrunesByAgeNotByMissingPath(t *testing.T) {
	dir, tmp := t.TempDir(), t.TempDir()
	gone, kept := filepath.Join(tmp, "gone.log"), filepath.Join(tmp, "kept.log")
	writeLog(t, gone, "x\n")
	writeLog(t, kept, "y\n")

	now := time.Now()
	clock := now
	opts := CheckpointOptions{Now: func() time.Time { return clock }}
	c := openStore(t, dir, opts)
	hGone, _ := c.Open(gone, true)
	hGone.Commit(fileID(t, gone), 2)
	c.Close()

	// The file vanishes (rotation window) but the entry is young: kept.
	os.Remove(gone)
	c = openStore(t, dir, opts)
	hKept, _ := c.Open(kept, true)
	hKept.Commit(fileID(t, kept), 2)
	c.Close()
	if !stateHas(t, dir, mustKey(t, kept)) {
		t.Fatal("kept entry missing")
	}
	if !stateHas(t, dir, canonicalMissing(gone)) {
		t.Error("young entry for a missing path was pruned; pruning must be by age only")
	}

	// A week later, untouched entries are pruned.
	clock = now.Add(8 * 24 * time.Hour)
	c = openStore(t, dir, opts)
	hKept, _ = c.Open(kept, true)
	hKept.Commit(fileID(t, kept), 2)
	c.Close()
	if stateHas(t, dir, canonicalMissing(gone)) {
		t.Error("stale entry survived the age cutoff")
	}
}

func TestCheckpoint_NilStoreFallsBackToLegacyBehaviour(t *testing.T) {
	var c *CheckpointStore
	h, start := c.Open("/nonexistent", true)
	if h != nil || start != -1 {
		t.Errorf("nil store startup = %v,%d want nil,-1", h, start)
	}
	if _, start := c.Open("/nonexistent", false); start != 0 {
		t.Errorf("nil store runtime = %d, want 0", start)
	}
	h.Commit(FileID{}, 5) // must not panic
	if c.Flush() != nil || c.Close() != nil {
		t.Error("nil store flush/close should be no-ops")
	}
}

// --- helpers ---

func mustKey(t *testing.T, path string) string {
	t.Helper()
	k, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	k, _ = filepath.Abs(k)
	return k
}

// canonicalMissing resolves the directory of a path that no longer exists
// (EvalSymlinks fails on the file itself, e.g. /var -> /private/var on macOS).
func canonicalMissing(path string) string {
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return path
	}
	return filepath.Join(dir, filepath.Base(path))
}

func stateHas(t *testing.T, dir, key string) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(data), `"`+key+`"`)
}
