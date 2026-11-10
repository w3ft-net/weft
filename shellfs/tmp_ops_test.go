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
	return New(db, cust.ID, nil, WithTier("swatch"), WithUser(addMember(t, db, cust.ID, name, ownerPerms())))
}

// cp and mv work across mounts, and the destination's own save checks still
// fire: the write lands in the mount's WriteFile, exactly as `cat x > y` does.
func TestCopyAndMove_AcrossMountsRunSaveChecks(t *testing.T) {
	fs := tmpOpsFS(t, "tmp-cpmv")
	if err := fs.WriteFile("/tmp/a", []byte("echo hi\n")); err != nil {
		t.Fatal(err)
	}
	if err := fs.CopyFile("/tmp/a", "/bin/c"); err != nil {
		t.Fatalf("cp tmp to bin: %v", err)
	}
	if got, err := fs.ReadFile("/bin/c"); err != nil || !strings.Contains(string(got), "echo hi") {
		t.Errorf("/bin/c = %q, %v", got, err)
	}
	if err := fs.CopyFile("/bin/c", "/tmp/c"); err != nil {
		t.Errorf("cp bin to tmp: %v", err)
	}
	if err := fs.Rename("/bin/c", "/tmp/d"); err != nil {
		t.Errorf("mv bin to tmp: %v", err)
	}
	if _, err := fs.Stat("/bin/c"); err == nil {
		t.Errorf("/bin/c should be gone after mv")
	}
	if err := fs.Rename("/tmp/d", "/bin/e"); err != nil {
		t.Errorf("mv tmp to bin: %v", err)
	}

	// A cron that does not parse is rejected by /cron.d's save check.
	if err := fs.WriteFile("/tmp/notacron", []byte("garbage\n")); err != nil {
		t.Fatal(err)
	}
	err := fs.CopyFile("/tmp/notacron", "/cron.d/bad")
	if err == nil {
		t.Errorf("cp of an unparsable cron into /cron.d must fail the save check")
	}
	if _, serr := fs.Stat("/cron.d/bad"); serr == nil {
		t.Errorf("/cron.d/bad exists after a failed save")
	}

	// /etc/passwd and /log stay read-only.
	for _, dst := range []string{"/etc/passwd", "/log/x"} {
		if err := fs.CopyFile("/tmp/a", dst); err == nil {
			t.Errorf("cp into %s must be refused", dst)
		}
	}
	if err := fs.Rename("/log/x", "/tmp/y"); err == nil {
		t.Errorf("mv out of read-only /log must be refused")
	}
}

// Without the group that manages /bin, cp and mv into it are refused just as
// a redirect is.
func TestCopyAndMove_RespectGroupPermissions(t *testing.T) {
	db := testStore(t)
	cust, err := db.FindOrCreateOrg(fmt.Sprintf("tmp-cpmv-member-%d", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	fs := New(db, cust.ID, nil, WithTier("swatch"), WithUser("u-member"))
	if err := fs.WriteFile("/tmp/a", []byte("echo hi\n")); err != nil {
		t.Fatal(err)
	}
	if err := fs.CopyFile("/tmp/a", "/bin/c"); err == nil {
		t.Errorf("cp into /bin by a non-member must be refused")
	}
	if err := fs.Rename("/tmp/a", "/bin/c"); err == nil {
		t.Errorf("mv into /bin by a non-member must be refused")
	}
	if _, err := fs.Stat("/tmp/a"); err != nil {
		t.Errorf("/tmp/a must survive a refused mv: %v", err)
	}
	if err := fs.WriteFile("/bin/c", []byte("echo hi\n")); err == nil {
		t.Errorf("a plain write is refused too; cp and mv must agree with it")
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
