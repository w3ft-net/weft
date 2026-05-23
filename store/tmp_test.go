package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// TestTmpRoundTrip covers the baseline: write a file, read it back,
// size and line count accurate.
func TestTmpRoundTrip(t *testing.T) {
	db := testOpen(t)
	cust, err := db.FindOrCreateCustomer("tmp-rt")
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("one\ntwo\nthree\n")
	if err := db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/nums", data); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readTmpFile(db, cust.ID, "/tmp/nums")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := "one\ntwo\nthree\n"
	if string(got) != want {
		t.Errorf("contents: got %q, want %q", got, want)
	}

	f, err := db.GetTmpFile(cust.ID, "/tmp/nums")
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if f.LineCount != 3 {
		t.Errorf("line_count: got %d, want 3", f.LineCount)
	}
	if f.ByteSize != int64(len(data)) {
		t.Errorf("byte_size: got %d, want %d", f.ByteSize, len(data))
	}
	if f.IsDir {
		t.Errorf("is_dir: got true, want false")
	}
}

// TestTmpOverwrite: a second write replaces, doesn't append.
func TestTmpOverwrite(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("tmp-ow")

	if err := db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/x", []byte("v1\n")); err != nil {
		t.Fatal(err)
	}
	if err := db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/x", []byte("v2\n")); err != nil {
		t.Fatal(err)
	}
	got, _ := readTmpFile(db, cust.ID, "/tmp/x")
	if string(got) != "v2\n" {
		t.Errorf("overwrite: got %q, want %q", got, "v2\n")
	}
}

// TestTmpAppend: >> keeps existing lines and numbers new ones after.
func TestTmpAppend(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("tmp-ap")

	if err := db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/log", []byte("one\ntwo\n")); err != nil {
		t.Fatal(err)
	}
	if err := appendTmpFile(db, cust.ID, TierSwatch, "", "/tmp/log", []byte("three\nfour\n")); err != nil {
		t.Fatal(err)
	}
	got, _ := readTmpFile(db, cust.ID, "/tmp/log")
	want := "one\ntwo\nthree\nfour\n"
	if string(got) != want {
		t.Errorf("append: got %q, want %q", got, want)
	}

	f, _ := db.GetTmpFile(cust.ID, "/tmp/log")
	if f.LineCount != 4 {
		t.Errorf("line_count after append: got %d, want 4", f.LineCount)
	}
}

// TestTmpMkdir: create a directory, write a file under it, ls that dir.
func TestTmpMkdir(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("tmp-mk")

	if err := db.Mkdir(cust.ID, "", "/tmp/incidents"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/incidents/a", []byte("hi\n")); err != nil {
		t.Fatalf("write under dir: %v", err)
	}
	children, err := db.ListTmpFiles(cust.ID, "/tmp/incidents")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].Path != "incidents/a" {
		t.Errorf("list children: got %+v, want one entry 'incidents/a'", children)
	}

	// Writing to a path whose parent doesn't exist should fail.
	if err := db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/nope/x", []byte("x")); !errors.Is(err, ErrTmpNotFound) {
		t.Errorf("write under missing parent: got %v, want ErrTmpNotFound", err)
	}
}

// TestTmpPerFileCap: writing > TmpMaxFileBytes fails.
func TestTmpPerFileCap(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("tmp-cap")

	big := make([]byte, TmpMaxFileBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	err := db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/big", big)
	if !errors.Is(err, ErrTmpFileTooLarge) {
		t.Errorf("per-file cap: got %v, want ErrTmpFileTooLarge", err)
	}
}

// TestTmpQuota: trial tier caps at 500 KB total.
func TestTmpQuota(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("tmp-quota")

	chunk := make([]byte, 200*1024)
	for i := range chunk {
		chunk[i] = 'y'
	}
	if err := db.WriteTmpFile(cust.ID, TierTrial, "", "/tmp/a", chunk); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := db.WriteTmpFile(cust.ID, TierTrial, "", "/tmp/b", chunk); err != nil {
		t.Fatalf("second write: %v", err)
	}
	// Third 200 KB push would put us at 600 KB > 500 KB trial cap.
	err := db.WriteTmpFile(cust.ID, TierTrial, "", "/tmp/c", chunk)
	if !errors.Is(err, ErrTmpQuotaExceeded) {
		t.Errorf("trial quota: got %v, want ErrTmpQuotaExceeded", err)
	}
}

// TestTmpDeleteSubtree: rm -rf a directory removes everything under it.
func TestTmpDeleteSubtree(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("tmp-rm")

	if err := db.Mkdir(cust.ID, "", "/tmp/inc"); err != nil {
		t.Fatal(err)
	}
	if err := db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/inc/a", []byte("a\n")); err != nil {
		t.Fatal(err)
	}
	if err := db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/inc/b", []byte("b\n")); err != nil {
		t.Fatal(err)
	}
	if err := deleteTmpSubtree(db, cust.ID, "/tmp/inc"); err != nil {
		t.Fatalf("rm -rf: %v", err)
	}
	if _, err := db.GetTmpFile(cust.ID, "/tmp/inc"); !errors.Is(err, ErrTmpNotFound) {
		t.Errorf("parent should be gone: got %v", err)
	}
	if _, err := db.GetTmpFile(cust.ID, "/tmp/inc/a"); !errors.Is(err, ErrTmpNotFound) {
		t.Errorf("child a should be gone: got %v", err)
	}
}

// TestTmpMultiTenancy: customer A cannot see customer B's tmp files.
func TestTmpMultiTenancy(t *testing.T) {
	db := testOpen(t)
	a, _ := db.FindOrCreateCustomer("tmp-a")
	b, _ := db.FindOrCreateCustomer("tmp-b")

	if err := db.WriteTmpFile(a.ID, TierSwatch, "", "/tmp/secret", []byte("top secret\n")); err != nil {
		t.Fatal(err)
	}
	// B tries to read A's path.
	if _, err := db.GetTmpFile(b.ID, "/tmp/secret"); !errors.Is(err, ErrTmpNotFound) {
		t.Errorf("cross-tenant read: got %v, want ErrTmpNotFound", err)
	}
	// B's listing is empty.
	rows, _ := db.ListTmpFiles(b.ID, "")
	if len(rows) != 0 {
		t.Errorf("cross-tenant list: got %+v, want empty", rows)
	}
}

// TestTmpInvalidPaths rejects path traversal and other garbage.
func TestTmpInvalidPaths(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("tmp-bad")

	for _, bad := range []string{"", "/", "/tmp/../etc/passwd", "/tmp//x", "/tmp/a/./b"} {
		err := db.WriteTmpFile(cust.ID, TierSwatch, "", bad, []byte("x"))
		if !errors.Is(err, ErrTmpInvalidPath) {
			t.Errorf("path %q: got %v, want ErrTmpInvalidPath", bad, err)
		}
	}
}

// TestTmpAppendExactCap: appending right up to the line is ok;
// appending one byte past fails.
func TestTmpAppendExactCap(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("tmp-cap-edge")

	half := make([]byte, TmpMaxFileBytes-1)
	for i := range half {
		half[i] = 'a'
	}
	if err := db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/f", half); err != nil {
		t.Fatal(err)
	}
	// One more byte to reach exactly the cap.
	if err := appendTmpFile(db, cust.ID, TierSwatch, "", "/tmp/f", []byte("b")); err != nil {
		t.Fatalf("append to exactly cap: %v", err)
	}
	// One byte over.
	err := appendTmpFile(db, cust.ID, TierSwatch, "", "/tmp/f", []byte("c"))
	if !errors.Is(err, ErrTmpFileTooLarge) {
		t.Errorf("append past cap: got %v, want ErrTmpFileTooLarge", err)
	}
}

// TestTmpCopy: cp duplicates contents and metadata; both files are
// independently readable; quota counts the copy.
func TestTmpCopy(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("tmp-cp")

	if err := db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/src", []byte("alpha\nbeta\ngamma\n")); err != nil {
		t.Fatal(err)
	}
	if err := copyTmpFile(db, cust.ID, TierSwatch, "", "/tmp/src", "/tmp/dst"); err != nil {
		t.Fatalf("cp: %v", err)
	}

	srcGot, _ := readTmpFile(db, cust.ID, "/tmp/src")
	dstGot, _ := readTmpFile(db, cust.ID, "/tmp/dst")
	if string(srcGot) != string(dstGot) {
		t.Errorf("cp: contents diverged: src=%q dst=%q", srcGot, dstGot)
	}
	if string(dstGot) != "alpha\nbeta\ngamma\n" {
		t.Errorf("cp dst: got %q, want %q", dstGot, "alpha\nbeta\ngamma\n")
	}
	// Mutate src; dst should be unaffected.
	if err := db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/src", []byte("changed\n")); err != nil {
		t.Fatal(err)
	}
	dstGot2, _ := readTmpFile(db, cust.ID, "/tmp/dst")
	if string(dstGot2) != "alpha\nbeta\ngamma\n" {
		t.Errorf("cp: dst tracked src mutation, got %q", dstGot2)
	}
}

// TestTmpCopy_RejectsExisting: cp to an existing path errors.
func TestTmpCopy_RejectsExisting(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("tmp-cp-exist")
	db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/a", []byte("a"))
	db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/b", []byte("b"))
	if err := copyTmpFile(db, cust.ID, TierSwatch, "", "/tmp/a", "/tmp/b"); !errors.Is(err, ErrTmpExists) {
		t.Errorf("cp to existing dst: got %v, want ErrTmpExists", err)
	}
}

// TestTmpCopy_QuotaEnforced: cp that would exceed the customer
// allowance is rejected before any rows land.
func TestTmpCopy_QuotaEnforced(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("tmp-cp-quota")
	chunk := make([]byte, 200*1024)
	for i := range chunk {
		chunk[i] = 'q'
	}
	if err := db.WriteTmpFile(cust.ID, TierTrial, "", "/tmp/a", chunk); err != nil {
		t.Fatal(err)
	}
	// Trial cap is 500 KB; another 200 KB fits, but a third 200 KB
	// copy would push us to 600 KB.
	if err := db.WriteTmpFile(cust.ID, TierTrial, "", "/tmp/b", chunk); err != nil {
		t.Fatal(err)
	}
	if err := copyTmpFile(db, cust.ID, TierTrial, "", "/tmp/a", "/tmp/c"); !errors.Is(err, ErrTmpQuotaExceeded) {
		t.Errorf("cp into full quota: got %v, want ErrTmpQuotaExceeded", err)
	}
}

// TestTmpRename: mv changes path; lines untouched (file_id stable).
func TestTmpRename(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("tmp-mv")

	db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/old", []byte("hello\n"))
	old, _ := db.GetTmpFile(cust.ID, "/tmp/old")

	if err := renameTmpFile(db, cust.ID, "/tmp/old", "/tmp/new"); err != nil {
		t.Fatalf("mv: %v", err)
	}
	if _, err := db.GetTmpFile(cust.ID, "/tmp/old"); !errors.Is(err, ErrTmpNotFound) {
		t.Errorf("old path should be gone: got %v", err)
	}
	moved, err := db.GetTmpFile(cust.ID, "/tmp/new")
	if err != nil {
		t.Fatal(err)
	}
	if moved.ID != old.ID {
		t.Errorf("mv should preserve file id: got %d, want %d", moved.ID, old.ID)
	}
	got, _ := readTmpFile(db, cust.ID, "/tmp/new")
	if string(got) != "hello\n" {
		t.Errorf("mv content: got %q, want hello\\n", got)
	}
}

// TestTmpTouch_Existing: touch bumps updated_at on metadata + lines.
// All lines slide forward into the same fresh partition as the file.
func TestTmpTouch_Existing(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("tmp-touch")

	db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/x", []byte("a\nb\nc\n"))
	before, _ := db.GetTmpFile(cust.ID, "/tmp/x")
	beforeTime := before.UpdatedAt

	// Sleep long enough that NOW() advances.
	time.Sleep(20 * time.Millisecond)
	if err := touchTmpFile(db, cust.ID, TierSwatch, "", "/tmp/x"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	after, _ := db.GetTmpFile(cust.ID, "/tmp/x")
	if !after.UpdatedAt.After(beforeTime) {
		t.Errorf("touch did not bump updated_at: before=%v after=%v", beforeTime, after.UpdatedAt)
	}
	// Confirm the file's content is still there — touch didn't
	// orphan its lines.
	got, _ := readTmpFile(db, cust.ID, "/tmp/x")
	if string(got) != "a\nb\nc\n" {
		t.Errorf("touch corrupted contents: got %q", got)
	}
}

// TestTmpTouch_Missing: touch on a missing file creates it empty.
func TestTmpTouch_Missing(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("tmp-touch-new")

	if err := touchTmpFile(db, cust.ID, TierSwatch, "", "/tmp/fresh"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	got, _ := readTmpFile(db, cust.ID, "/tmp/fresh")
	if len(got) != 0 {
		t.Errorf("fresh touched file: got %q, want empty", got)
	}
	f, _ := db.GetTmpFile(cust.ID, "/tmp/fresh")
	if f.LineCount != 0 {
		t.Errorf("fresh touched file line_count: got %d, want 0", f.LineCount)
	}
}

// TestTmpUsageBytes sums only files, not directory rows.
func TestTmpUsageBytes(t *testing.T) {
	db := testOpen(t)
	cust, _ := db.FindOrCreateCustomer("tmp-usage")

	if err := db.Mkdir(cust.ID, "", "/tmp/d"); err != nil {
		t.Fatal(err)
	}
	if err := db.WriteTmpFile(cust.ID, TierSwatch, "", "/tmp/d/file", []byte(strings.Repeat("z", 1000))); err != nil {
		t.Fatal(err)
	}
	used, err := db.TmpUsageBytes(cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	if used != 1000 {
		t.Errorf("usage: got %d, want 1000", used)
	}
}

// --- helpers for tmp store methods relocated out of the store package ---
//
// The production logic for these /tmp operations moved to callers
// outside the store package. These test-local helpers reproduce the
// original method bodies verbatim (parameterized on db) so the existing
// store tests keep exercising the same behaviour against the schema.

// readTmpFile mirrors the former Store.ReadTmpFile.
func readTmpFile(db *Store, customerID uint, path string) ([]byte, error) {
	f, err := db.GetTmpFile(customerID, path)
	if err != nil {
		return nil, err
	}
	if f.IsDir {
		return nil, ErrTmpIsDir
	}
	if f.LineCount == 0 {
		return []byte{}, nil
	}
	var lines []TmpFileLine
	if err := db.db.Where("file_id = ?", f.ID).
		Order("line_num").Find(&lines).Error; err != nil {
		return nil, err
	}
	var b strings.Builder
	b.Grow(int(f.ByteSize))
	for i, ln := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(ln.Content)
	}
	if int64(b.Len()) < f.ByteSize {
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}

// appendTmpFile mirrors the former Store.AppendTmpFile.
func appendTmpFile(db *Store, customerID uint, tier Tier, userID, path string, data []byte) error {
	p, err := NormalizeTmpPath(path)
	if err != nil {
		return err
	}
	if err := db.EnsureParentDirs(customerID, userID, p, false); err != nil {
		return err
	}

	existing, err := db.GetTmpFile(customerID, p)
	if err != nil && !errors.Is(err, ErrTmpNotFound) {
		return err
	}
	if existing != nil && existing.IsDir {
		return ErrTmpIsDir
	}

	var existingSize int64
	var startLine int64
	if existing != nil {
		existingSize = existing.ByteSize
		startLine = existing.LineCount
	}
	newSize := existingSize + int64(len(data))
	if newSize > TmpMaxFileBytes {
		return fmt.Errorf("%w (%d + %d > %d)", ErrTmpFileTooLarge, existingSize, len(data), TmpMaxFileBytes)
	}

	allowance := TmpStorageAllowance(tier)
	used, err := db.TmpUsageBytes(customerID)
	if err != nil {
		return err
	}
	if used+int64(len(data)) > allowance {
		return fmt.Errorf("%w (%d + %d > %d)", ErrTmpQuotaExceeded, used, len(data), allowance)
	}

	lines := SplitLines(data)
	return db.scoped(customerID).Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		var fileID uint64
		if existing != nil {
			fileID = existing.ID
			if err := tx.Model(&TmpFile{}).
				Where("id = ? AND updated_at = ?", existing.ID, existing.UpdatedAt).
				Updates(map[string]any{
					"byte_size":  newSize,
					"line_count": startLine + int64(len(lines)),
					"updated_at": now,
				}).Error; err != nil {
				return err
			}
			if err := tx.Exec(
				"UPDATE tmp_file_lines SET updated_at = ? WHERE file_id = ?",
				now, fileID,
			).Error; err != nil {
				return err
			}
		} else {
			f := TmpFile{
				CustomerID: customerID,
				Path:       p,
				IsDir:      false,
				ByteSize:   newSize,
				LineCount:  int64(len(lines)),
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			if userID != "" {
				f.CreatedByUserID = &userID
			}
			if err := tx.Create(&f).Error; err != nil {
				return err
			}
			fileID = f.ID
		}
		if len(lines) == 0 {
			return nil
		}
		rows := make([]TmpFileLine, len(lines))
		for i, line := range lines {
			rows[i] = TmpFileLine{
				FileID:    fileID,
				LineNum:   startLine + int64(i+1),
				Content:   line,
				UpdatedAt: now,
			}
		}
		return tx.CreateInBatches(rows, 500).Error
	})
}

// copyTmpFile mirrors the former Store.CopyTmpFile.
func copyTmpFile(db *Store, customerID uint, tier Tier, userID, srcPath, dstPath string) error {
	src, err := db.GetTmpFile(customerID, srcPath)
	if err != nil {
		return err
	}
	if src.IsDir {
		return fmt.Errorf("%w: cannot copy a directory", ErrTmpIsDir)
	}
	dstNorm, err := NormalizeTmpPath(dstPath)
	if err != nil {
		return err
	}
	if existing, _ := db.GetTmpFile(customerID, dstNorm); existing != nil {
		return ErrTmpExists
	}
	if err := db.EnsureParentDirs(customerID, userID, dstNorm, false); err != nil {
		return err
	}

	if src.ByteSize > TmpMaxFileBytes {
		return fmt.Errorf("%w (source %d > %d)", ErrTmpFileTooLarge, src.ByteSize, TmpMaxFileBytes)
	}
	allowance := TmpStorageAllowance(tier)
	used, err := db.TmpUsageBytes(customerID)
	if err != nil {
		return err
	}
	if used+src.ByteSize > allowance {
		return fmt.Errorf("%w (%d + %d > %d)", ErrTmpQuotaExceeded, used, src.ByteSize, allowance)
	}

	return db.scoped(customerID).Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		dst := TmpFile{
			CustomerID: customerID,
			Path:       dstNorm,
			IsDir:      false,
			ByteSize:   src.ByteSize,
			LineCount:  src.LineCount,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if userID != "" {
			dst.CreatedByUserID = &userID
		}
		if err := tx.Create(&dst).Error; err != nil {
			return err
		}
		return tx.Exec(`
			INSERT INTO tmp_file_lines (file_id, line_num, content, updated_at)
			SELECT ?, line_num, content, ?
			FROM tmp_file_lines
			WHERE file_id = ?
		`, dst.ID, now, src.ID).Error
	})
}

// renameTmpFile mirrors the former Store.RenameTmpFile.
func renameTmpFile(db *Store, customerID uint, srcPath, dstPath string) error {
	src, err := db.GetTmpFile(customerID, srcPath)
	if err != nil {
		return err
	}
	dstNorm, err := NormalizeTmpPath(dstPath)
	if err != nil {
		return err
	}
	if existing, _ := db.GetTmpFile(customerID, dstNorm); existing != nil {
		return ErrTmpExists
	}
	if err := db.EnsureParentDirs(customerID, "", dstNorm, false); err != nil {
		return err
	}
	now := time.Now()
	return db.scoped(customerID).Model(&TmpFile{}).
		Where("id = ? AND updated_at = ?", src.ID, src.UpdatedAt).
		Updates(map[string]any{
			"path":       dstNorm,
			"updated_at": now,
		}).Error
}

// touchTmpFile mirrors the former Store.TouchTmpFile.
func touchTmpFile(db *Store, customerID uint, tier Tier, userID, path string) error {
	p, err := NormalizeTmpPath(path)
	if err != nil {
		return err
	}
	existing, err := db.GetTmpFile(customerID, p)
	if errors.Is(err, ErrTmpNotFound) {
		return db.WriteTmpFile(customerID, tier, userID, p, nil)
	}
	if err != nil {
		return err
	}
	now := time.Now()
	return db.scoped(customerID).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&TmpFile{}).
			Where("id = ? AND updated_at = ?", existing.ID, existing.UpdatedAt).
			Update("updated_at", now).Error; err != nil {
			return err
		}
		if existing.IsDir || existing.LineCount == 0 {
			return nil
		}
		return tx.Exec(
			"UPDATE tmp_file_lines SET updated_at = ? WHERE file_id = ?",
			now, existing.ID,
		).Error
	})
}

// deleteTmpSubtree mirrors the former Store.DeleteTmpSubtree.
func deleteTmpSubtree(db *Store, customerID uint, path string) error {
	p, err := NormalizeTmpPath(path)
	if err != nil {
		return err
	}
	return db.scoped(customerID).Transaction(func(tx *gorm.DB) error {
		var files []TmpFile
		err := tx.Where("path = ? OR path LIKE ?",
			p, p+"/%").Find(&files).Error
		if err != nil {
			return err
		}
		if len(files) == 0 {
			return ErrTmpNotFound
		}
		ids := make([]uint64, len(files))
		for i, f := range files {
			ids[i] = f.ID
		}
		if err := tx.Where("file_id IN ?", ids).Delete(&TmpFileLine{}).Error; err != nil {
			return err
		}
		return tx.Where("id IN ?", ids).Delete(&TmpFile{}).Error
	})
}
