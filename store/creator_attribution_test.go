package store

import (
	"testing"
)

// These tests pin the creator-attribution contract: every Save / Write
// path on the three user-content namespaces (/bin via shell_functions,
// /cron.d via cron_watches, /tmp via tmp_files) must record the user
// performing the write so `ls -al` can later show ownership. Empty
// userID is permitted (system writes; pre-existing rows; the cron
// executor) and stores as NULL.

func TestSaveShellFunction_RecordsCreator(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("creator-bin")
	user, err := db.CreateUser("alice-bin@example.test")
	if err != nil {
		t.Fatal(err)
	}

	if err := db.SaveShellFunction(cust.ID, user.ID, "scan", "echo hi", "bash"); err != nil {
		t.Fatal(err)
	}
	fn, err := db.GetShellFunction(cust.ID, "scan")
	if err != nil {
		t.Fatal(err)
	}
	if fn.CreatedByUserID == nil || *fn.CreatedByUserID != user.ID {
		t.Errorf("CreatedByUserID: got %v, want %q", fn.CreatedByUserID, user.ID)
	}
}

func TestSaveShellFunction_EmptyUserStoresNull(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("creator-bin-empty")

	if err := db.SaveShellFunction(cust.ID, "", "scan", "echo hi", "bash"); err != nil {
		t.Fatal(err)
	}
	fn, _ := db.GetShellFunction(cust.ID, "scan")
	if fn.CreatedByUserID != nil {
		t.Errorf("empty userID should store NULL; got %q", *fn.CreatedByUserID)
	}
}

func TestSaveCronWatch_RecordsCreator(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("creator-cron")
	user, err := db.CreateUser("alice-cron@example.test")
	if err != nil {
		t.Fatal(err)
	}

	if err := db.SaveCronWatch(cust.ID, user.ID, "errors", "*/5 * * * *", "echo 1", nil); err != nil {
		t.Fatal(err)
	}
	w, err := db.GetCronWatch(cust.ID, "errors")
	if err != nil {
		t.Fatal(err)
	}
	if w.CreatedByUserID == nil || *w.CreatedByUserID != user.ID {
		t.Errorf("CreatedByUserID: got %v, want %q", w.CreatedByUserID, user.ID)
	}
}

func TestWriteTmpFile_RecordsCreator(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("creator-tmp")
	user, err := db.CreateUser("alice-tmp@example.test")
	if err != nil {
		t.Fatal(err)
	}

	if err := db.WriteTmpFile(cust.ID, TierSwatch, user.ID, "/tmp/scratch", []byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	f, err := db.GetTmpFile(cust.ID, "/tmp/scratch")
	if err != nil {
		t.Fatal(err)
	}
	if f.CreatedByUserID == nil || *f.CreatedByUserID != user.ID {
		t.Errorf("CreatedByUserID: got %v, want %q", f.CreatedByUserID, user.ID)
	}
}

// TestAppendTmpFile_PreservesCreator: appending should NOT change the
// creator. The original author still owns the file even if a colleague
// added a few lines.
func TestAppendTmpFile_PreservesCreator(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("creator-tmp-append")
	alice, _ := db.CreateUser("alice-app@example.test")
	bob, _ := db.CreateUser("bob-app@example.test")

	if err := db.WriteTmpFile(cust.ID, TierSwatch, alice.ID, "/tmp/notes", []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if err := appendTmpFile(db, cust.ID, TierSwatch, bob.ID, "/tmp/notes", []byte("second\n")); err != nil {
		t.Fatal(err)
	}
	f, _ := db.GetTmpFile(cust.ID, "/tmp/notes")
	if f.CreatedByUserID == nil || *f.CreatedByUserID != alice.ID {
		t.Errorf("after append by bob, creator should still be alice; got %v", f.CreatedByUserID)
	}
}

// TestCopyTmpFile_RecordsCopierAsCreator: cp records the user doing
// the copy, not the source's creator. The new file is a new artifact
// and the copying user is its author.
func TestCopyTmpFile_RecordsCopierAsCreator(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("creator-tmp-copy")
	alice, _ := db.CreateUser("alice-cp@example.test")
	bob, _ := db.CreateUser("bob-cp@example.test")

	if err := db.WriteTmpFile(cust.ID, TierSwatch, alice.ID, "/tmp/src", []byte("data\n")); err != nil {
		t.Fatal(err)
	}
	if err := copyTmpFile(db, cust.ID, TierSwatch, bob.ID, "/tmp/src", "/tmp/dst"); err != nil {
		t.Fatal(err)
	}
	dst, _ := db.GetTmpFile(cust.ID, "/tmp/dst")
	if dst.CreatedByUserID == nil || *dst.CreatedByUserID != bob.ID {
		t.Errorf("copy: dst creator should be bob (the copier); got %v", dst.CreatedByUserID)
	}
	src, _ := db.GetTmpFile(cust.ID, "/tmp/src")
	if src.CreatedByUserID == nil || *src.CreatedByUserID != alice.ID {
		t.Errorf("copy: src creator should still be alice; got %v", src.CreatedByUserID)
	}
}

// TestRenameTmpFile_PreservesCreator: mv keeps the original creator.
func TestRenameTmpFile_PreservesCreator(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("creator-tmp-mv")
	alice, _ := db.CreateUser("alice-mv@example.test")

	if err := db.WriteTmpFile(cust.ID, TierSwatch, alice.ID, "/tmp/old", []byte("x\n")); err != nil {
		t.Fatal(err)
	}
	if err := renameTmpFile(db, cust.ID, "/tmp/old", "/tmp/new"); err != nil {
		t.Fatal(err)
	}
	f, _ := db.GetTmpFile(cust.ID, "/tmp/new")
	if f.CreatedByUserID == nil || *f.CreatedByUserID != alice.ID {
		t.Errorf("mv: creator should still be alice; got %v", f.CreatedByUserID)
	}
}

func TestMkdir_RecordsCreator(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateOrg("creator-tmp-mkdir")
	alice, _ := db.CreateUser("alice-mkdir@example.test")

	if err := db.Mkdir(cust.ID, alice.ID, "/tmp/projects"); err != nil {
		t.Fatal(err)
	}
	d, _ := db.GetTmpFile(cust.ID, "/tmp/projects")
	if d.CreatedByUserID == nil || *d.CreatedByUserID != alice.ID {
		t.Errorf("mkdir: creator should be alice; got %v", d.CreatedByUserID)
	}
}
