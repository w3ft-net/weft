//go:build !defaultshape

package mounts

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/alerting"
	"github.com/graybeardcorp/weft/tenancy/hosted/alerts"
)

const notifySystem = "system:\n  email: [ops@example.test]\n"

// Saving /etc/notify.yaml checks the kinds against what the build sends: the
// open build sends webhook and email, so `slack:` is refused at save time, not
// skipped when an alert fires.
func TestEtcWriteNotify_RefusesAKindTheBuildDoesNotSend(t *testing.T) {
	m := &EtcMount{cfg: Config{}}
	err := m.WriteFile("notify.yaml", []byte(notifySystem+"ops:\n  slack: ['#ops']\n"))
	if err == nil || !strings.Contains(err.Error(), `"slack"`) {
		t.Fatalf("err = %v, want a refusal naming slack", err)
	}
}

// A deployment's own kinds are accepted, and their entries are still checked.
func TestEtcWriteNotify_AcceptsTheDeploymentsKindsAndChecksTheirEntries(t *testing.T) {
	kinds := alerting.NewRegistry(nil, alerts.Targets(nil, nil)...)

	bad := &EtcMount{cfg: Config{AlertKinds: kinds}}
	err := bad.WriteFile("notify.yaml", []byte(notifySystem+"ops:\n  pagerduty:\n    - {name: primary, severity: loud}\n"))
	if err == nil || !strings.Contains(err.Error(), "severity") {
		t.Fatalf("a bad PagerDuty severity: err = %v, want it refused", err)
	}

	cfg := ownerCfg(t, nil)
	cfg.AlertKinds = kinds
	good := &EtcMount{cfg: cfg}
	if err := good.WriteFile("notify.yaml", []byte(notifySystem+"ops:\n  slack: ['#ops']\n  pagerduty: [primary]\n")); err != nil {
		t.Fatalf("hosted kinds were refused: %v", err)
	}
}
