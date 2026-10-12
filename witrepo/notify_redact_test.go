package witrepo

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/store"
)

// git log -p, show and blame present version content through versionContent,
// so a redacted `cat` must not be undone by reading history.
func TestVersionContent_RedactsNotifySecrets(t *testing.T) {
	src := "ops:\n  webhook:\n    - url: https://hooks.example.com/a\n      headers:\n        Authorization: \"Bearer s3cr3t-token\"\n"
	got := versionContent(store.ObjectVersion{Kind: "etc", Name: "notify.yaml", Source: src})
	if strings.Contains(got, "s3cr3t-token") || !strings.Contains(got, store.NotifyRedacted) {
		t.Errorf("history must show the placeholder, not the secret:\n%s", got)
	}
	// Dashboards are not credentials and are left alone.
	dash := "title: x\n"
	if versionContent(store.ObjectVersion{Kind: "etc/dashboards", Name: "x.yaml", Source: dash}) != dash {
		t.Error("dashboard history must be unchanged")
	}
}
