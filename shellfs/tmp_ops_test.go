package shellfs

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func tmpOpsFS(t *testing.T, name string) *ShellFS {
	t.Helper()
	db := testStore(t)
	cust, err := db.FindOrCreateOrg(fmt.Sprintf("%s-%d", name, os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	return New(db, cust.ID, nil, WithTier("swatch"), WithUser("u-"+name, ownerPerms()))
}

// cp and mv work inside /tmp only, on both sides (docs/manual/tmp.md).
// Anywhere else the write must go through its own save path:
// `cat /tmp/x > /bin/y`. The mount layer used to allow any pair of
// mounts, so `cp /bin/scan /tmp/x` and `cp /tmp/x /bin/y` both ran.
func TestCopyAndMove_OnlyInsideTmp(t *testing.T) {
	fs := tmpOpsFS(t, "tmp-cpmv")
	if err := fs.WriteFile("/tmp/a", []byte("x\n")); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("/bin/scan", []byte("echo scan\n")); err != nil {
		t.Fatal(err)
	}

	refused := []struct {
		name string
		do   func() error
	}{
		{"cp bin to tmp", func() error { return fs.CopyFile("/bin/scan", "/tmp/c") }},
		{"cp tmp to bin", func() error { return fs.CopyFile("/tmp/a", "/bin/c") }},
		{"mv tmp to bin", func() error { return fs.Rename("/tmp/a", "/bin/c") }},
		{"mv bin to tmp", func() error { return fs.Rename("/bin/scan", "/tmp/d") }},
	}
	for _, tc := range refused {
		err := tc.do()
		if err == nil || !strings.Contains(err.Error(), "only inside /tmp") {
			t.Errorf("%s: err = %v, want a refusal saying cp and mv work only inside /tmp", tc.name, err)
		}
	}
	// Refused means untouched: nothing moved, nothing copied.
	if _, err := fs.Stat("/bin/scan"); err != nil {
		t.Errorf("/bin/scan must survive a refused mv: %v", err)
	}
	if _, err := fs.Stat("/tmp/a"); err != nil {
		t.Errorf("/tmp/a must survive a refused mv: %v", err)
	}
	for _, p := range []string{"/tmp/c", "/tmp/d", "/bin/c"} {
		if _, err := fs.Stat(p); err == nil {
			t.Errorf("%s exists after a refused copy or move", p)
		}
	}

	if err := fs.CopyFile("/tmp/a", "/tmp/b"); err != nil {
		t.Errorf("cp inside /tmp: %v", err)
	}
	if err := fs.Rename("/tmp/b", "/tmp/e"); err != nil {
		t.Errorf("mv inside /tmp: %v", err)
	}
}

// touch resets the 48h clock: the modification time moves forward.
func TestTouch_MovesModificationTime(t *testing.T) {
	fs := tmpOpsFS(t, "tmp-touch")
	if err := fs.WriteFile("/tmp/t", []byte("x\n")); err != nil {
		t.Fatal(err)
	}
	before, err := fs.Stat("/tmp/t")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := fs.Touch("/tmp/t"); err != nil {
		t.Fatal(err)
	}
	after, err := fs.Stat("/tmp/t")
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().After(before.ModTime()) {
		t.Errorf("touch did not move the mtime: before %v, after %v", before.ModTime(), after.ModTime())
	}
}
