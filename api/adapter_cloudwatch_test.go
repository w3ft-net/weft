package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCloudWatchAdapter_Auth_HeaderKey: X-Amz-Firehose-Access-Key is the
// bearer-equivalent, per Firehose's HTTP endpoint delivery spec.
func TestCloudWatchAdapter_Auth_HeaderKey(t *testing.T) {
	a := CloudWatchAdapter{}
	req := httptest.NewRequest(http.MethodPost, "/api/ingest/cloudwatch", nil)
	req.Header.Set("X-Amz-Firehose-Access-Key", "wft_abcdef")
	got, err := a.Auth(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != "wft_abcdef" {
		t.Errorf("token = %q, want wft_abcdef", got)
	}
}

func TestCloudWatchAdapter_Auth_Missing(t *testing.T) {
	a := CloudWatchAdapter{}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if _, err := a.Auth(req); err == nil {
		t.Error("expected error for missing access key header")
	}
}
