package store

import (
	"strings"
	"testing"
)

const notifyWithSecrets = `# routing for the on-call rotation
prod-oncall:
  slack:
    - "#ops"
  webhook:
    - url: https://hooks.example.com/a
      headers:
        Authorization: "Bearer s3cr3t-token"
        X-Signature-Secret: whsec_abc123
    - https://plain.example.com/b
  email:
    - oncall@example.com
`

// Header values are credentials (an Authorization token, a signing secret):
// reading notify.yaml shows a placeholder, never the value. Everything else
// - URLs, header names, channels, comments - stays readable.
func TestRedactNotifyYAML(t *testing.T) {
	got := RedactNotifyYAML(notifyWithSecrets)
	for _, secret := range []string{"s3cr3t-token", "whsec_abc123", "Bearer"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted output still holds %q:\n%s", secret, got)
		}
	}
	for _, keep := range []string{"# routing for the on-call rotation", "https://hooks.example.com/a", "Authorization", "X-Signature-Secret", "#ops", "oncall@example.com", "https://plain.example.com/b"} {
		if !strings.Contains(got, keep) {
			t.Errorf("redacted output lost %q:\n%s", keep, got)
		}
	}
	if n := strings.Count(got, NotifyRedacted); n != 2 {
		t.Errorf("want 2 placeholders, got %d:\n%s", n, got)
	}
	// Still valid notify.yaml.
	if _, err := ParseNotifyAliases(got); err != nil {
		t.Errorf("redacted output no longer parses: %v", err)
	}
	// Nothing to redact: unchanged, byte for byte.
	plain := "ops:\n  slack:\n    - \"#ops\"\n"
	if RedactNotifyYAML(plain) != plain {
		t.Error("a document with no headers must come back unchanged")
	}
	if RedactNotifyYAML("") != "" || RedactNotifyYAML("{not yaml") != "{not yaml" {
		t.Error("empty and unparseable input must come back unchanged")
	}
}

// Editing a redacted file and saving it keeps the stored secrets for every
// header still showing the placeholder, and takes a new value as written.
func TestRestoreNotifyRedactions(t *testing.T) {
	redacted := RedactNotifyYAML(notifyWithSecrets)

	// Edit something unrelated; leave the placeholders alone.
	edited := strings.Replace(redacted, `"#ops"`, `"#incidents"`, 1)
	got, err := RestoreNotifyRedactions(edited, notifyWithSecrets)
	if err != nil {
		t.Fatal(err)
	}
	aliases, err := ParseNotifyAliases(got)
	if err != nil {
		t.Fatal(err)
	}
	hdr := aliases["prod-oncall"].Webhook[0].Headers
	if hdr["Authorization"] != "Bearer s3cr3t-token" || hdr["X-Signature-Secret"] != "whsec_abc123" {
		t.Errorf("stored secrets not restored: %v", hdr)
	}
	if aliases["prod-oncall"].Slack[0].Channel != "#incidents" {
		t.Errorf("the edit was lost: %+v", aliases["prod-oncall"].Slack)
	}

	// Replace one secret with a new value; the other still restores.
	rotated := strings.Replace(edited, `Authorization: "<redacted>"`, `Authorization: "Bearer rotated"`, 1)
	if rotated == edited {
		t.Fatalf("test setup: could not find the Authorization placeholder in:\n%s", edited)
	}
	got, err = RestoreNotifyRedactions(rotated, notifyWithSecrets)
	if err != nil {
		t.Fatal(err)
	}
	aliases, _ = ParseNotifyAliases(got)
	hdr = aliases["prod-oncall"].Webhook[0].Headers
	if hdr["Authorization"] != "Bearer rotated" || hdr["X-Signature-Secret"] != "whsec_abc123" {
		t.Errorf("rotation: headers = %v", hdr)
	}

	// No placeholders: the text is saved exactly as written, not re-encoded.
	exact := "# mine\nops:\n\n  slack:   [\"#ops\"]\n"
	if got, err := RestoreNotifyRedactions(exact, notifyWithSecrets); err != nil || got != exact {
		t.Errorf("text without placeholders must pass through unchanged: %q, %v", got, err)
	}
}

// A placeholder with no stored value behind it (a new webhook, a changed
// URL, a renamed alias) is an error naming it, never saved as a literal
// "<redacted>" credential.
func TestRestoreNotifyRedactions_PlaceholderWithNothingStored(t *testing.T) {
	moved := strings.Replace(RedactNotifyYAML(notifyWithSecrets), "https://hooks.example.com/a", "https://hooks.example.com/moved", 1)
	_, err := RestoreNotifyRedactions(moved, notifyWithSecrets)
	if err == nil {
		t.Fatal("a placeholder with no stored value must be refused")
	}
	for _, want := range []string{"Authorization", "https://hooks.example.com/moved", "prod-oncall"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q: %v", want, err)
		}
	}
	if _, err := RestoreNotifyRedactions(RedactNotifyYAML(notifyWithSecrets), ""); err == nil {
		t.Error("placeholders with no stored config at all must be refused")
	}
}
