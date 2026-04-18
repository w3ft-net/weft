package store

import (
	"strings"
	"testing"
)

func TestParseNotifyAliases_Empty(t *testing.T) {
	m, err := ParseNotifyAliases("")
	if err != nil {
		t.Fatalf("empty input should parse: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("want empty map, got %v", m)
	}
}

func TestParseNotifyAliases_Basic(t *testing.T) {
	raw := `
ops:
  slack:
    - "#ops-alerts"
    - "#backend"
  pagerduty:
    - name: ops-daytime
      severity: error
    - name: ops-weekend
`
	m, err := ParseNotifyAliases(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ops, ok := m["ops"]
	if !ok {
		t.Fatal("missing alias `ops`")
	}
	if len(ops.Slack) != 2 || ops.Slack[0].Channel != "#ops-alerts" || ops.Slack[1].Channel != "#backend" {
		t.Errorf("slack channels wrong: %+v", ops.Slack)
	}
	if len(ops.PagerDuty) != 2 {
		t.Fatalf("want 2 PD targets, got %d", len(ops.PagerDuty))
	}
	if ops.PagerDuty[0].Name != "ops-daytime" || ops.PagerDuty[0].Severity != "error" {
		t.Errorf("pd[0] wrong: %+v", ops.PagerDuty[0])
	}
	// Severity defaults to "error" when omitted.
	if ops.PagerDuty[1].Name != "ops-weekend" || ops.PagerDuty[1].Severity != "error" {
		t.Errorf("pd[1] severity should default to error: %+v", ops.PagerDuty[1])
	}
}

func TestParseNotifyAliases_InvalidSeverity(t *testing.T) {
	raw := `
critical:
  pagerduty:
    - name: pager
      severity: nuclear
`
	_, err := ParseNotifyAliases(raw)
	if err == nil || !strings.Contains(err.Error(), "severity") {
		t.Fatalf("want severity error, got %v", err)
	}
}

func TestParseNotifyAliases_EmptyAlias(t *testing.T) {
	raw := `
empty:
  slack: []
`
	_, err := ParseNotifyAliases(raw)
	if err == nil || !strings.Contains(err.Error(), "no targets") {
		t.Fatalf("want 'no targets' error, got %v", err)
	}
}

func TestParseNotifyAliases_BareStringShorthand(t *testing.T) {
	// Both Slack channels and PD integrations should accept bare-string form.
	raw := `
simple:
  slack:
    - "#alerts"
  pagerduty:
    - oncall
`
	m, err := ParseNotifyAliases(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	simple := m["simple"]
	if simple.Slack[0].Channel != "#alerts" {
		t.Errorf("slack bare string didn't parse: %+v", simple.Slack[0])
	}
	if simple.PagerDuty[0].Name != "oncall" || simple.PagerDuty[0].Severity != "error" {
		t.Errorf("pd bare string didn't parse: %+v", simple.PagerDuty[0])
	}
}
