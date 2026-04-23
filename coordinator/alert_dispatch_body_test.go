package coordinator

import (
	"strings"
	"testing"
)

// recordingNotifier captures the body handed to Send so tests can assert
// on exactly what would be shipped to Slack/PagerDuty.
type recordingNotifier struct {
	sent []struct {
		customerID uint
		alias      string
		watch      string
		body       string
	}
}

func (r *recordingNotifier) Send(customerID uint, alias, watch, body string) error {
	r.sent = append(r.sent, struct {
		customerID uint
		alias      string
		watch      string
		body       string
	}{customerID, alias, watch, body})
	return nil
}

// TestAlertDispatch_BodyIsFormattedMessageNotRawRow — regression guard.
// Before this fix the notifier got `r.raw` (the raw stdin row that
// triggered the alert, e.g. "40" for a scalar or "5.12 disk" for a
// multi-row tick). Users expected the formatted message the shell
// prints to stderr ("weft: 40.0 > 30.0" / "watchname[disk]: …") since
// that's the version that names the watch, the tag, and the threshold.
func TestAlertDispatch_BodyIsFormattedMessageNotRawRow(t *testing.T) {
	rec := &recordingNotifier{}
	ctx := &AlertContext{
		Notifier:    rec,
		CustomerID:  1,
		Interactive: true,
	}

	var stdout, stderr strings.Builder
	code := AlertCommand([]string{"above", "30", "--notify=foo"}, strings.NewReader("40\n"), &stdout, &stderr, ctx)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, stderr.String())
	}

	if len(rec.sent) != 1 {
		t.Fatalf("want exactly 1 dispatch, got %d: %+v", len(rec.sent), rec.sent)
	}
	got := rec.sent[0].body
	if got == "40" {
		t.Fatalf("body is the raw stdin row (regression): %q", got)
	}
	want := "weft: 40.0 > 30.0"
	if got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestAlertDispatch_BodyIncludesTagInMultiRowMode — the multi-row variant.
// Each tag dispatches its own alert; the body must carry the tag so the
// PagerDuty incident / Slack line identifies which thing fired.
func TestAlertDispatch_BodyIncludesTagInMultiRowMode(t *testing.T) {
	rec := &recordingNotifier{}
	ctx := &AlertContext{
		Notifier:    rec,
		CustomerID:  1,
		Interactive: true,
	}

	// stdin: two rows, value in field 1, tag in field 2. Alert above 5.
	input := "7 disk\n3 cpu\n"
	args := []string{"above", "5", "-k", "1", "-t", "2", "--notify=foo"}

	var stdout, stderr strings.Builder
	code := AlertCommand(args, strings.NewReader(input), &stdout, &stderr, ctx)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, stderr.String())
	}

	if len(rec.sent) != 1 {
		t.Fatalf("want 1 dispatch (only 'disk' exceeds 5), got %d: %+v", len(rec.sent), rec.sent)
	}
	got := rec.sent[0].body
	if !strings.Contains(got, "disk") {
		t.Errorf("body %q missing tag 'disk'", got)
	}
	if !strings.Contains(got, "7.0 > 5.0") {
		t.Errorf("body %q missing threshold reason", got)
	}
	if strings.Contains(got, "cpu") {
		t.Errorf("body %q leaked the non-firing 'cpu' row", got)
	}
}
