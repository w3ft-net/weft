//go:build !defaultshape

package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/store"
)

func TestSendWebhook(t *testing.T) {
	var received webhookPayload
	var receivedHeaders http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	n := NewNotifier(nil, nil)

	target := store.WebhookTarget{
		URL:     srv.URL,
		Headers: map[string]string{"X-Signing-Secret": "test-secret"},
	}

	if err := n.sendWebhook(context.Background(), "ops", "error-rate", target, "error-rate: 127 (above 50)"); err != nil {
		t.Fatalf("sendWebhook: %v", err)
	}

	if received.V != 2 {
		t.Errorf("want v=2, got %d", received.V)
	}
	if received.Watch != "error-rate" {
		t.Errorf("want cron=error-rate, got %q", received.Watch)
	}
	if received.Alias != "ops" {
		t.Errorf("want alias=ops, got %q", received.Alias)
	}
	if received.Body != "error-rate: 127 (above 50)" {
		t.Errorf("body wrong: %q", received.Body)
	}
	if receivedHeaders.Get("Content-Type") != "application/json" {
		t.Errorf("content-type wrong: %q", receivedHeaders.Get("Content-Type"))
	}
	if receivedHeaders.Get("X-Signing-Secret") != "test-secret" {
		t.Errorf("custom header missing: %q", receivedHeaders.Get("X-Signing-Secret"))
	}
}

func TestSendWebhookNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	n := NewNotifier(nil, nil)

	target := store.WebhookTarget{URL: srv.URL}
	err := n.sendWebhook(context.Background(), "ops", "error-rate", target, "test")
	if err == nil {
		t.Fatal("expected error for non-2xx response")
	}
}

func TestSendEmailNoConfig(t *testing.T) {
	n := NewNotifier(nil, nil)
	target := store.EmailTarget{Address: "test@example.com"}
	err := n.sendEmail(context.Background(), "error-rate", target, "test body")
	if err == nil {
		t.Fatal("expected error when no email config")
	}
}

// v2 of the webhook payload names the cron `cron` (v1 called it `watch`).
// The wire key is what consumers read, so pin the JSON itself.
func TestWebhookPayload_CronKeyAndVersion(t *testing.T) {
	b, err := json.Marshal(webhookPayload{V: 2, Watch: "error-rate", Alias: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, `"cron":"error-rate"`) || strings.Contains(got, `"watch"`) || !strings.Contains(got, `"v":2`) {
		t.Errorf("payload = %s, want v=2 with a cron key and no watch key", got)
	}
}

// A webhook target's ${secrets.NAME} references are filled in at send time:
// the endpoint sees the real values, and nothing that comes back out of Send
// (an error carries the request URL) does.
func TestSendWebhookInterpolatesSecrets(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	var gotAuth, gotKey string
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotKey = r.Header.Get("Authorization"), r.URL.Query().Get("key")
		if fail {
			http.Error(w, "no", 500)
		}
	}))
	defer srv.Close()

	for name, v := range map[string]string{"token": "tok-123", "api_key": "key-456"} {
		if err := db.SaveWebhookSecret(custID, name, v); err != nil {
			t.Fatal(err)
		}
	}
	yaml := "system:\n  email:\n    - ops@example.test\nhook:\n  webhook:\n    - url: \"" + srv.URL + "/in?key=${secrets.api_key}\"\n      headers:\n        Authorization: \"Bearer ${secrets.token}\"\n"
	if err := db.SaveNotifyAliases(custID, "", yaml); err != nil {
		t.Fatal(err)
	}

	n := NewNotifier(db, nil)
	if _, err := n.Send(context.Background(), custID, "hook", "disk", "boom", nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotAuth != "Bearer tok-123" || gotKey != "key-456" {
		t.Errorf("endpoint saw Authorization=%q key=%q, want the secrets filled in", gotAuth, gotKey)
	}

	fail = true
	_, err := n.Send(context.Background(), custID, "hook", "disk", "boom", nil)
	if err == nil {
		t.Fatal("a 500 was not an error")
	}
	if strings.Contains(err.Error(), "key-456") || strings.Contains(err.Error(), "tok-123") {
		t.Errorf("error leaks a secret: %v", err)
	}

	// A refused connection is the error that quotes the whole URL.
	srv.Close()
	_, err = n.Send(context.Background(), custID, "hook", "disk", "boom", nil)
	if err == nil || !strings.Contains(err.Error(), "${secrets.api_key}") || strings.Contains(err.Error(), "key-456") {
		t.Errorf("connection error = %v, want the URL shown with its placeholder, not the secret", err)
	}
}
