package store

import (
	"strings"
	"testing"
)

const secretNotifyYAML = `system:
  email:
    - ops@example.test
hook:
  webhook:
    - url: "https://api.example.test/alerts?key=${secrets.api_key}"
      headers:
        Authorization: "Bearer ${secrets.token}"
        X-Static: plain
`

func TestWebhookSecrets_SaveListUpdateDelete(t *testing.T) {
	db := testOpen(t)
	org, _ := db.FindOrCreateOrg("wh-secret-crud")

	if err := db.SaveWebhookSecret(org.ID, "token", "one"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveWebhookSecret(org.ID, "token", "two"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if err := db.SaveWebhookSecret(org.ID, "api_key", "k"); err != nil {
		t.Fatal(err)
	}
	names, err := db.ListWebhookSecretNames(org.ID)
	if err != nil || strings.Join(names, ",") != "api_key,token" {
		t.Fatalf("names = %v, err = %v; want api_key,token", names, err)
	}

	got, _, err := db.ResolveWebhookTarget(org.ID, WebhookTarget{URL: "https://x/${secrets.token}"})
	if err != nil || got.URL != "https://x/two" {
		t.Fatalf("resolved %q, err %v; want the overwritten value", got.URL, err)
	}

	for _, bad := range []string{"", "has space", "a.b", "${x}"} {
		if err := db.SaveWebhookSecret(org.ID, bad, "v"); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if err := db.SaveWebhookSecret(org.ID, "empty", ""); err == nil {
		t.Error("empty value accepted")
	}

	if err := db.DeleteWebhookSecret(org.ID, "token"); err != nil {
		t.Fatal(err)
	}
	// Gone for good: the value isn't parked in a soft-deleted row.
	var n int64
	db.DB().Raw(`SELECT count(*) FROM webhook_secrets WHERE org_id = ? AND name = 'token'`, org.ID).Scan(&n)
	if n != 0 {
		t.Errorf("deleted secret still has %d row(s)", n)
	}
	if err := db.DeleteWebhookSecret(org.ID, "token"); err == nil {
		t.Error("deleting a missing secret succeeded")
	}
}

func TestWebhookSecrets_AreOrgScoped(t *testing.T) {
	db := testOpen(t)
	a, _ := db.FindOrCreateOrg("wh-secret-a")
	b, _ := db.FindOrCreateOrg("wh-secret-b")
	if err := db.SaveWebhookSecret(a.ID, "token", "a-value"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ResolveWebhookTarget(b.ID, WebhookTarget{URL: "https://x/${secrets.token}"}); err == nil {
		t.Fatal("org b resolved org a's secret")
	}
}

func TestSaveNotifyAliases_RequiresDefinedSecrets(t *testing.T) {
	db := testOpen(t)
	org, _ := db.FindOrCreateOrg("wh-secret-validate")

	err := db.SaveNotifyAliases(org.ID, "", secretNotifyYAML)
	if err == nil || !strings.Contains(err.Error(), "api_key") && !strings.Contains(err.Error(), "token") {
		t.Fatalf("save with undefined secrets: err = %v, want one naming the missing secret", err)
	}
	for _, n := range []string{"api_key", "token"} {
		if err := db.SaveWebhookSecret(org.ID, n, "v-"+n); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SaveNotifyAliases(org.ID, "", secretNotifyYAML); err != nil {
		t.Fatalf("save with defined secrets: %v", err)
	}

	// Can't pull a secret out from under the alias that uses it.
	err = db.DeleteWebhookSecret(org.ID, "token")
	if err == nil || !strings.Contains(err.Error(), "hook") {
		t.Fatalf("delete of a referenced secret: err = %v, want a refusal naming alias hook", err)
	}

	got, redact, err := db.ResolveWebhookTarget(org.ID, WebhookTarget{
		URL:     "https://api.example.test/alerts?key=${secrets.api_key}",
		Headers: map[string]string{"Authorization": "Bearer ${secrets.token}", "X-Static": "plain"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://api.example.test/alerts?key=v-api_key" || got.Headers["Authorization"] != "Bearer v-token" || got.Headers["X-Static"] != "plain" {
		t.Errorf("resolved = %+v", got)
	}
	if s := redact.Replace("Post " + got.URL + ": timeout"); strings.Contains(s, "v-api_key") {
		t.Errorf("redactor left the secret in %q", s)
	}
}
