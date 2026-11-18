package mounts

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/w3ft-net/weft/alerting"
)

const notifySystem = "system:\n  email: [ops@example.test]\n"

// Saving /etc/notify.yaml checks the kinds against what the build sends: the
// open build sends webhook and email, so `slack:` is refused at save time, not
// skipped when an alert fires.
func TestEtcWriteNotify_RefusesAKindTheBuildDoesNotSend(t *testing.T) {
	m := &EtcMount{cfg: Config{}}
	err := m.WriteFile(context.Background(), "notify.yaml", []byte(notifySystem+"ops:\n  slack: ['#ops']\n"))
	if err == nil || !strings.Contains(err.Error(), `"slack"`) {
		t.Fatalf("err = %v, want a refusal naming slack", err)
	}
}

// pagerTarget is a kind a deployment registers itself: an entry is a name or a
// mapping with a severity, which must be one of a few.
type pagerTarget struct{}

func (pagerTarget) Kind() string { return "pager" }

func (pagerTarget) Open(entry *yaml.Node) (alerting.Destination, error) {
	var t struct {
		Name     string `yaml:"name"`
		Severity string `yaml:"severity"`
	}
	if entry.Kind == yaml.ScalarNode {
		t.Name = entry.Value
	} else if err := entry.Decode(&t); err != nil {
		return alerting.Destination{}, err
	}
	switch t.Severity {
	case "", "critical", "error", "warning", "info":
	default:
		return alerting.Destination{}, fmt.Errorf("severity %q is not one of critical, error, warning, info", t.Severity)
	}
	return alerting.Destination{
		Key: "pager:" + t.Name, Name: t.Name,
		Send: func(context.Context, alerting.Alert) error { return nil },
	}, nil
}

// A deployment's own kinds are accepted, and their entries are still checked.
func TestEtcWriteNotify_AcceptsTheDeploymentsKindsAndChecksTheirEntries(t *testing.T) {
	kinds := alerting.NewRegistry(nil, pagerTarget{})

	bad := &EtcMount{cfg: Config{AlertKinds: kinds}}
	err := bad.WriteFile(context.Background(), "notify.yaml", []byte(notifySystem+"ops:\n  pager:\n    - {name: primary, severity: loud}\n"))
	if err == nil || !strings.Contains(err.Error(), "severity") {
		t.Fatalf("a bad severity: err = %v, want it refused", err)
	}

	cfg := ownerCfg(t, nil)
	cfg.AlertKinds = kinds
	good := &EtcMount{cfg: cfg}
	if err := good.WriteFile(context.Background(), "notify.yaml", []byte(notifySystem+"ops:\n  pager: [primary]\n")); err != nil {
		t.Fatalf("the deployment's kind was refused: %v", err)
	}
	if doc, _ := cfg.Notify.Aliases.Aliases(cfg.Owner); !strings.Contains(doc.Text, "pager") {
		t.Errorf("the saved document = %q", doc.Text)
	}
}
