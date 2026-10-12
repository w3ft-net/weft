package coordinator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/store"
)

// readTmpFile reconstructs the byte contents of a tmp file via the
// exported store surface. It mirrors the logic that formerly lived in
// store.ReadTmpFile, which was relocated into production callers.
func readTmpFile(db *store.Store, orgID store.OrgID, path string) ([]byte, error) {
	f, err := db.GetTmpFile(orgID, path)
	if err != nil {
		return nil, err
	}
	if f.IsDir {
		return nil, store.ErrTmpIsDir
	}
	if f.LineCount == 0 {
		return []byte{}, nil
	}
	var lines []store.TmpFileLine
	if err := db.DB().Where("file_id = ?", f.ID).
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
	// If the writer supplied a trailing newline, byte_size > joined
	// length by exactly one. Restore it so `cat` round-trips cleanly.
	if int64(b.Len()) < f.ByteSize {
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}

// TestCoordinatorQuery_LogQueryRedirectToTmp covers the second shape
// the user flagged: a full log-query pipeline whose output is
// redirected into /tmp. The fix for `cat /etc/...` routes through
// runLocal (warp+shellfs), but `cat /log/<app>.log | awk ... >
// /tmp/errs` is a real log query — its output never reaches FS write
// machinery unless the redirect is honoured by the coordinator. The
// test asserts both that the redirect lands the filtered rows in
// /tmp and that the result is "consumed" by the redirect (so the
// shell doesn't double-print the data).
//
// The pipeline uses awk's LEVEL field-equality (a stable pushdown
// path in this codebase — see TestQuery_AwkLevelFilter) rather than
// `grep ERROR`, which depends on DuckDB regex behaviour that varies
// across builds and is unrelated to the redirect logic under test.
func TestCoordinatorQuery_LogQueryRedirectToTmp(t *testing.T) {
	c := setupTestCoordinator(t)
	const tmpPath = "/tmp/errs"

	res, err := c.Execute(context.Background(), `cat /log/vimgym.log | awk 'LEVEL == "ERROR"' > `+tmpPath,
		fullRange, "kevlar", "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	resD := drainExec(t, res)
	if resD.Text != "" || len(resD.Rows) != 0 {
		t.Errorf("redirected query should leave the response empty (the file is the sink); got Text=%q rows=%d",
			resD.Text, len(resD.Rows))
	}

	cust, err := c.Store.GetOrgByName("kevlar")
	if err != nil {
		t.Fatal(err)
	}
	data, err := readTmpFile(c.Store, cust.ID, tmpPath)
	if err != nil {
		t.Fatalf("read /tmp/errs back: %v (the redirect was probably dropped)", err)
	}
	body := string(data)
	// Shard 0 has vimgym ERROR lines: 1, 4, 9. Shard 1 has: 17, 23.
	for _, want := range []string{
		"ERROR vimgym i-001 connection timeout",
		"ERROR vimgym i-001 null pointer exception",
		"ERROR vimgym i-003 timeout waiting for response",
		"ERROR vimgym i-001 500 internal server error",
		"ERROR vimgym i-001 connection timeout to upstream",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/tmp/errs missing %q\nfile contents:\n%s", want, body)
		}
	}
	// Negative checks: the filter ran, so we should NOT see INFO
	// vimgym lines or rows from the other-customer fixture.
	if strings.Contains(body, "INFO vimgym") {
		t.Errorf("/tmp/errs contains INFO lines — filter not applied:\n%s", body)
	}
	if strings.Contains(body, "i-999") {
		t.Errorf("/tmp/errs contains other-customer rows:\n%s", body)
	}
}

// TestCoordinatorQuery_CatRedirectFromEtcToTmp covers the user-reported
// flow: `cat /etc/notify.yaml > /tmp/notify.yaml` should write the
// /etc file's contents into /tmp via shellfs. Today the planner sees
// `cat /etc/notify.yaml` and routes it as a log query against the
// (non-existent) source app `/etc/notify.yaml`, so the coordinator
// rejects it with "unknown source: /etc/notify.yaml" and the
// follow-up `cat /tmp/notify.yaml` returns nothing because nothing
// was ever written.
func TestCoordinatorQuery_CatRedirectFromEtcToTmp(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	const conf = "weft-info:\n  slack:\n    - \"#weft-auto\"\n"
	if err := db.SaveNotifyAliases(custID, "", conf); err != nil {
		t.Fatalf("seed /etc/notify.yaml: %v", err)
	}

	c := NewCoordinator(CoordinatorConfig{Pool: &compute.LocalComputePool{}, Store: db})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	cust, _ := db.GetOrgByID(custID)

	res, err := c.Execute(context.Background(), `cat /etc/notify.yaml > /tmp/notify.yaml`, tr, cust.Name, "")
	if err != nil {
		t.Fatalf("cat-redirect Query: %v", err)
	}
	drainExec(t, res)

	row, err := db.GetTmpFile(custID, "/tmp/notify.yaml")
	if err != nil {
		t.Fatalf("read back /tmp/notify.yaml: %v", err)
	}
	got, err := readTmpFile(db, custID, "/tmp/notify.yaml")
	if err != nil {
		t.Fatalf("ReadTmpFile: %v", err)
	}
	if string(got) != conf {
		t.Errorf("/tmp/notify.yaml contents:\n got: %q\nwant: %q", string(got), conf)
	}
	if row.ByteSize != int64(len(conf)) {
		t.Errorf("/tmp/notify.yaml byte_size = %d, want %d", row.ByteSize, len(conf))
	}
}

// TestCoordinatorQuery_CatTmpReturnsContent covers the second half of
// the repro: once /tmp/notify.yaml exists, `cat /tmp/notify.yaml`
// should stream the content back as Text/rows, not bounce off the
// source-allowlist with "unknown source".
func TestCoordinatorQuery_CatTmpReturnsContent(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	const body = "hello from tmp\n"
	if err := db.WriteTmpFile(custID, store.TierBolt, "", "/tmp/note", []byte(body)); err != nil {
		t.Fatalf("seed /tmp/note: %v", err)
	}

	c := NewCoordinator(CoordinatorConfig{Pool: &compute.LocalComputePool{}, Store: db})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	cust, _ := db.GetOrgByID(custID)

	res, err := c.Execute(context.Background(), `cat /tmp/note`, tr, cust.Name, "")
	if err != nil {
		t.Fatalf("cat /tmp/note: %v", err)
	}
	resD := drainExec(t, res)
	got := resD.Text
	if got == "" {
		var sb strings.Builder
		for _, r := range resD.Rows {
			sb.WriteString(r.Raw)
			sb.WriteByte('\n')
		}
		got = sb.String()
	}
	if !strings.Contains(got, "hello from tmp") {
		t.Errorf("cat /tmp/note: got %q, want it to contain %q", got, "hello from tmp")
	}
}
