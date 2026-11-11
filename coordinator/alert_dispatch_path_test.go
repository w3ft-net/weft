package coordinator

import (
	"context"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/store"
)

// TestAlertDispatch_NamesCronByPath: two namespaces can each hold a cron called
// `disk-use`. The name handed to the notifier builds the PagerDuty dedup key and
// the webhook `cron` field, so it must be <namespace>/<name> or the two crons'
// incidents collapse into one.
func TestAlertDispatch_NamesCronByPath(t *testing.T) {
	db, org := testStoreAndCustomer(t)
	var names []string
	for _, nsName := range []string{"web", "api"} {
		ns, err := db.FindOrCreateNamespace(org, nsName)
		if err != nil {
			t.Fatal(err)
		}
		rec := &recordingNotifier{}
		ac := &AlertContext{
			Store:    db,
			Notifier: rec,
			OrgID:    org,
			Watch:    &store.CronWatch{Name: "disk-use", OrgID: org, NamespaceID: &ns.ID},
		}
		var stdout, stderr strings.Builder
		AlertCommand(context.Background(), []string{"above", "30", "--notify=foo"}, strings.NewReader("40\n"), &stdout, &stderr, ac)
		if len(rec.sent) != 1 {
			t.Fatalf("%s: want 1 dispatch, got %d", nsName, len(rec.sent))
		}
		names = append(names, rec.sent[0].watch)
	}
	if names[0] != "web/disk-use" || names[1] != "api/disk-use" {
		t.Fatalf("dispatched names %v, want [web/disk-use api/disk-use]", names)
	}
}
