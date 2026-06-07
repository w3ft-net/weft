package coordinator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	if err := n.sendWebhook("ops", "error-rate", target, "error-rate: 127 (above 50)"); err != nil {
		t.Fatalf("sendWebhook: %v", err)
	}

	if received.V != 1 {
		t.Errorf("want v=1, got %d", received.V)
	}
	if received.Watch != "error-rate" {
		t.Errorf("want watch=error-rate, got %q", received.Watch)
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
	err := n.sendWebhook("ops", "error-rate", target, "test")
	if err == nil {
		t.Fatal("expected error for non-2xx response")
	}
}

func TestSendEmailNoConfig(t *testing.T) {
	n := NewNotifier(nil, nil)
	target := store.EmailTarget{Address: "test@example.com"}
	err := n.sendEmail("error-rate", target, "test body")
	if err == nil {
		t.Fatal("expected error when no email config")
	}
}
