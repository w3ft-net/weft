package notifycfg_test

import (
	"github.com/w3ft-net/weft/backends/mem"
	"strings"
	"testing"

	"github.com/w3ft-net/weft/notifycfg"
	"github.com/w3ft-net/weft/shape"
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

func newService(t *testing.T) notifycfg.Service {
	t.Helper()
	s := mem.NewNotify()
	return notifycfg.Service{Aliases: s, Secrets: s}
}

var owner shape.Added

func TestSave_RequiresTheSystemAliasAndDefinedSecrets(t *testing.T) {
	svc := newService(t)

	if err := svc.Save(owner, "", "ops:\n  email:\n    - a@x.test\n"); err == nil || !strings.Contains(err.Error(), `"system" alias is required`) {
		t.Fatalf("save without a system alias: %v", err)
	}
	err := svc.Save(owner, "", secretNotifyYAML)
	if err == nil || !strings.Contains(err.Error(), "api_key") && !strings.Contains(err.Error(), "token") {
		t.Fatalf("save with undefined secrets: err = %v, want one naming the missing secret", err)
	}
	for _, n := range []string{"api_key", "token"} {
		if err := svc.SetSecret(owner, n, "v-"+n); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.Save(owner, "", secretNotifyYAML); err != nil {
		t.Fatalf("save with defined secrets: %v", err)
	}
	if got, _ := svc.Aliases.Aliases(owner); got.Text != secretNotifyYAML {
		t.Fatalf("stored %q", got)
	}
	if m, err := svc.Current(owner); err != nil || m["hook"] == nil {
		t.Fatalf("Current = %v %v", m, err)
	}
}

func TestSecrets_NamesValuesAndTheReferenceGuard(t *testing.T) {
	svc := newService(t)
	for _, bad := range []string{"", "has space", "a.b", "x/y"} {
		if err := svc.SetSecret(owner, bad, "v"); err == nil {
			t.Errorf("secret name %q was accepted", bad)
		}
	}
	if err := svc.SetSecret(owner, "empty", ""); err == nil {
		t.Error("an empty value was accepted")
	}
	for _, n := range []string{"api_key", "token"} {
		if err := svc.SetSecret(owner, n, "v-"+n); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.Save(owner, "", secretNotifyYAML); err != nil {
		t.Fatal(err)
	}

	// Can't pull a secret out from under the alias that uses it.
	if err := svc.DeleteSecret(owner, "token"); err == nil || !strings.Contains(err.Error(), "hook") {
		t.Fatalf("delete of a referenced secret: err = %v, want a refusal naming alias hook", err)
	}
	if err := svc.SetSecret(owner, "spare", "x"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSecret(owner, "spare"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSecret(owner, "spare"); err == nil || !strings.Contains(err.Error(), "no webhook secret") {
		t.Fatalf("delete of a missing secret: %v", err)
	}

	got, redact, err := svc.ResolveWebhook(owner, notifycfg.WebhookTarget{
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
	if _, _, err := svc.ResolveWebhook(owner, notifycfg.WebhookTarget{URL: "https://x/${secrets.nope}"}); err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Errorf("resolve of an undefined secret: %v", err)
	}
	if plain, _, err := svc.ResolveWebhook(owner, notifycfg.WebhookTarget{URL: "https://x/plain"}); err != nil || plain.URL != "https://x/plain" {
		t.Errorf("a target with no references = %+v %v", plain, err)
	}
}
