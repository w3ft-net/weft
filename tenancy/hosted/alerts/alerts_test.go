package alerts_test

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/graybeardcorp/weft/alerting"
	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted/alerts"
)

// The hosted notifier answers to every kind the aliases of existing orgs use,
// and each one's key is what was recorded in the delivered map before targets
// were pluggable.
func TestTargets_KeysAreTheStoredOnes(t *testing.T) {
	n := alerting.NewRegistry(nil, alerts.Targets(nil, nil)...)
	aliases, err := store.ParseNotifyAliases(`
ops:
  slack: ["#ops", {channel: "#dev"}]
  pagerduty: [primary, {name: backup, severity: critical}]
  email: [a@example.test]
  webhook: ["https://x.test/hook"]
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Validate(aliases); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	want := map[string]bool{
		"slack:#ops": true, "slack:#dev": true,
		"pagerduty:primary": true, "pagerduty:backup": true,
		"email:a@example.test": true,
	}
	got := map[string]bool{}
	for _, k := range aliases["ops"].Targets() {
		switch k.Kind {
		case "webhook":
			continue // the core's; its key is a digest, pinned in the alerting tests
		}
		tg := map[string]alerting.AlertTarget{}
		for _, a := range alerts.Targets(nil, nil) {
			tg[a.Kind()] = a
		}
		for _, e := range k.Entries {
			d, err := tg[k.Kind].Open(e)
			if err != nil {
				t.Fatal(err)
			}
			got[d.Key] = true
		}
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing key %q; got %v", k, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
}

func TestPagerDuty_SeverityDefaultsAndIsChecked(t *testing.T) {
	var n yaml.Node
	if err := n.Encode(map[string]string{"name": "p", "severity": "loud"}); err != nil {
		t.Fatal(err)
	}
	if _, err := (alerts.PagerDuty{}).Open(&n); err == nil {
		t.Error("an invalid severity opened")
	}
}
