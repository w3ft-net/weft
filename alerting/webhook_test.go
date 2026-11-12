package alerting

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

	w := NewWebhookTarget(nil)

	target := store.WebhookTarget{
		URL:     srv.URL,
		Headers: map[string]string{"X-Signing-Secret": "test-secret"},
	}

	if err := w.send(context.Background(), Alert{Alias: "ops", Cron: "error-rate", Body: "error-rate: 127 (above 50)"}, target); err != nil {
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

	w := NewWebhookTarget(nil)

	target := store.WebhookTarget{URL: srv.URL}
	err := w.send(context.Background(), Alert{Alias: "ops", Cron: "error-rate", Body: "test"}, target)
	if err == nil {
		t.Fatal("expected error for non-2xx response")
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
