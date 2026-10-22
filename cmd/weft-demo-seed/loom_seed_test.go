package main

import (
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// Loom's notify.yaml carries a reference to a webhook secret, never the
// value, and loom's crons get headroom over the default run budget.
func TestPreseedScripts_LoomSecretAndBudget(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	s, rollback := db.BeginTest()
	t.Cleanup(func() {
		rollback()
		db.Close()
	})

	org, err := s.FindOrCreateOrg("loom")
	if err != nil {
		t.Fatal(err)
	}
	var members []*store.User
	for _, email := range []string{"ops@loom.test", "kai@loom.test", "priya@loom.test"} {
		u, err := s.CreateUser(email)
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, u)
	}
	if err := preseedScripts(s, org.ID, "loom", members); err != nil {
		t.Fatalf("preseedScripts: %v", err)
	}

	nc, err := s.GetNotifyConfig(org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nc.AliasesYAML, "${secrets.oncall_signing}") || strings.Contains(nc.AliasesYAML, "loom-demo-secret") {
		t.Errorf("notify.yaml should reference the secret and not hold its value:\n%s", nc.AliasesYAML)
	}
	if names, _ := s.ListWebhookSecretNames(org.ID); len(names) != 1 || names[0] != "oncall_signing" {
		t.Errorf("secrets = %v, want [oncall_signing]", names)
	}
	if d, _ := s.CronRunTimeout(org.ID); d != 10*time.Second {
		t.Errorf("loom run budget = %v, want 10s", d)
	}
}
