package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const notifyKindsSystem = "system:\n  email: [oncall@example.test]\n"

// An api with no deployment kinds is the open build: it sends webhook, slack, pagerduty and email,
// so a save naming another kind is refused rather than accepted and skipped when an
// alert fires.
func TestNotifyConfig_OpenBuildRefusesAKindItDoesNotSend(t *testing.T) {
	a := newOpenAPI(t)
	body, _ := json.Marshal(map[string]string{"aliases_yaml": notifyKindsSystem + "ops:\n  smoke-signal: [hill]\n"})
	w := a.do(http.MethodPut, "/api/notify", body)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"smoke-signal"`) {
		t.Fatalf("PUT smoke-signal = %d %q, want 400 naming slack", w.Code, w.Body.String())
	}
	body, _ = json.Marshal(map[string]string{"aliases_yaml": notifyKindsSystem + "ops:\n  webhook: ['https://example.test/hook']\n"})
	if w := a.do(http.MethodPut, "/api/notify", body); w.Code != http.StatusOK {
		t.Fatalf("PUT webhook = %d %q, want 200", w.Code, w.Body.String())
	}
}
