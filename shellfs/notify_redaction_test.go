package shellfs

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

const notifyYAMLWithSecret = `ops:
  slack:
    - "#ops"
  webhook:
    - url: https://hooks.example.com/a
      headers:
        Authorization: "Bearer s3cr3t-token"
`

// /etc/notify.yaml shows no credentials: header values read as a
// placeholder, the size reported by ls matches what cat returns, and saving
// the file back with the placeholder intact keeps the stored secret.
func TestNotifyYAML_SecretsAreRedactedOnRead(t *testing.T) {
	db := testStore(t)
	cust, err := db.FindOrCreateOrg(fmt.Sprintf("notify-redact-%d", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	fs := New(db, cust.ID, nil, WithTier("swatch"), WithUser("u-notify", ownerPerms()))

	if err := fs.WriteFile("/etc/notify.yaml", []byte(notifyYAMLWithSecret)); err != nil {
		t.Fatal(err)
	}
	read, err := fs.ReadFile("/etc/notify.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(read), "s3cr3t-token") || !strings.Contains(string(read), "<redacted>") {
		t.Fatalf("notify.yaml must not show the secret:\n%s", read)
	}
	if fi, err := fs.Stat("/etc/notify.yaml"); err != nil || fi.Size() != int64(len(read)) {
		t.Errorf("Stat size %v (%v), want the %d bytes cat returns", fi, err, len(read))
	}

	// Edit through the redacted view and save: the secret survives.
	edited := strings.Replace(string(read), `"#ops"`, `"#incidents"`, 1)
	if err := fs.WriteFile("/etc/notify.yaml", []byte(edited)); err != nil {
		t.Fatalf("saving the redacted view back: %v", err)
	}
	nc, err := db.GetNotifyConfig(cust.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nc.AliasesYAML, "Bearer s3cr3t-token") || !strings.Contains(nc.AliasesYAML, "#incidents") {
		t.Errorf("stored config lost the secret or the edit:\n%s", nc.AliasesYAML)
	}

	// A placeholder with nothing stored behind it is refused, not saved.
	bad := strings.Replace(string(read), "https://hooks.example.com/a", "https://hooks.example.com/new", 1)
	if err := fs.WriteFile("/etc/notify.yaml", []byte(bad)); err == nil || !strings.Contains(err.Error(), "no secret is stored") {
		t.Errorf("placeholder with no stored secret: err = %v", err)
	}
}
