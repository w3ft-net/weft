package weftd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Phase 0 of docs/plans/weftd-offset-checkpointing.md: the tailer must be
// byte-accurate (offsets are the foundation checkpoints stand on) and must
// not drop data across truncation, rotation, or a late open.

const tailWait = 3 * time.Second

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func nextLine(t *testing.T, tl *Tailer) Line {
	t.Helper()
	select {
	case l, ok := <-tl.Lines():
		if !ok {
			t.Fatal("lines channel closed")
		}
		return l
	case <-time.After(tailWait):
		t.Fatal("timed out waiting for a line")
	}
	return Line{}
}

func expectNoLine(t *testing.T, tl *Tailer) {
	t.Helper()
	select {
	case l := <-tl.Lines():
		t.Fatalf("unexpected line %q", l.Text)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestTailer_PartialLineHeldUntilNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	appendFile(t, path, "")
	tl, err := NewTailer(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()

	appendFile(t, path, "abc")
	expectNoLine(t, tl)
	appendFile(t, path, "def\n")

	l := nextLine(t, tl)
	if l.Text != "abcdef" || l.End != 7 {
		t.Fatalf("got %q End=%d, want %q End=7", l.Text, l.End, "abcdef")
	}
	expectNoLine(t, tl)
}

func TestTailer_CRLFOffsetsAreTrueBytePositions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	appendFile(t, path, "")
	tl, err := NewTailer(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()

	appendFile(t, path, "one\r\ntwo\r\n")
	l1, l2 := nextLine(t, tl), nextLine(t, tl)
	if l1.Text != "one" || l1.End != 5 {
		t.Errorf("l1 = %q End=%d, want one/5", l1.Text, l1.End)
	}
	if l2.Text != "two" || l2.End != 10 {
		t.Errorf("l2 = %q End=%d, want two/10", l2.Text, l2.End)
	}
}

func TestTailer_LongLineDoesNotDesync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	appendFile(t, path, "")
	tl, err := newTailer(path, -1, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()

	long := strings.Repeat("x", 200*1024)
	appendFile(t, path, long+"\nnormal\n")

	l1 := nextLine(t, tl)
	if !strings.HasPrefix(l1.Text, strings.Repeat("x", 1024)) || !strings.Contains(l1.Text, "truncated") {
		t.Errorf("long line not truncated with marker: len=%d", len(l1.Text))
	}
	if l1.End != int64(len(long)+1) {
		t.Errorf("long line End=%d, want %d", l1.End, len(long)+1)
	}
	l2 := nextLine(t, tl)
	if l2.Text != "normal" || l2.End != int64(len(long)+1+7) {
		t.Errorf("after long line got %q End=%d, want normal/%d", l2.Text, l2.End, len(long)+8)
	}
}

func TestTailer_CopytruncateWhileRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	appendFile(t, path, "")
	tl, err := NewTailer(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()

	appendFile(t, path, "before-1\nbefore-2\n")
	nextLine(t, tl)
	nextLine(t, tl)

	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, "after\n")

	l := nextLine(t, tl)
	if l.Text != "after" || l.End != 6 {
		t.Fatalf("got %q End=%d, want after/6", l.Text, l.End)
	}
}

func TestTailer_RenameRotationDrainsOldTailFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	appendFile(t, path, "")
	tl, err := NewTailer(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()

	// Write and rotate back-to-back so the tailer cannot have drained yet.
	appendFile(t, path, "old-1\nold-2\n")
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, "new-1\n")

	var got []string
	for i := 0; i < 3; i++ {
		got = append(got, nextLine(t, tl).Text)
	}
	if strings.Join(got, ",") != "old-1,old-2,new-1" {
		t.Fatalf("got %v, want old tail then new file", got)
	}
}

func TestTailer_StartZeroReadsExistingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	appendFile(t, path, "pre-1\npre-2\n")
	tl, err := NewTailer(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()

	if l := nextLine(t, tl); l.Text != "pre-1" || l.End != 6 {
		t.Errorf("l1 = %q End=%d", l.Text, l.End)
	}
	if l := nextLine(t, tl); l.Text != "pre-2" || l.End != 12 {
		t.Errorf("l2 = %q End=%d", l.Text, l.End)
	}
}

func TestTailer_StartEOFSkipsExistingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	appendFile(t, path, "pre\n")
	tl, err := NewTailer(path, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()

	expectNoLine(t, tl)
	appendFile(t, path, "post\n")
	if l := nextLine(t, tl); l.Text != "post" || l.End != 9 {
		t.Errorf("got %q End=%d, want post/9", l.Text, l.End)
	}
}

func TestTailer_LinesCarryFileIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	appendFile(t, path, strings.Repeat("a", 2000)+"\n")
	tl, err := NewTailer(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()

	l := nextLine(t, tl)
	if l.File.Ino == 0 || l.File.FP == "" || l.File.FPLen != 1024 {
		t.Errorf("identity not populated: %+v", l.File)
	}
}

func TestTailer_FingerprintCompletesAsSmallFileGrows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	appendFile(t, path, "")
	tl, err := NewTailer(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()

	appendFile(t, path, "tiny\n")
	first := nextLine(t, tl)
	appendFile(t, path, strings.Repeat("y", 1500)+"\n")
	big := nextLine(t, tl)
	if big.File.FPLen != 1024 {
		t.Fatalf("FPLen = %d after file passed 1 KiB, want 1024 (short prefix would match almost any file)", big.File.FPLen)
	}
	if big.File.FP == first.File.FP {
		t.Error("fingerprint did not change when the prefix was extended")
	}
}
