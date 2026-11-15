package apitest

import (
	"strings"
	"testing"
)

// Bins runs the routes that read /bin scripts.
func Bins(t *testing.T, newSubject func(t *testing.T) Subject) {
	each(t, newSubject, []namedCase{
		{"GetFunctionReturnsPlainSource", getFunctionPlainSource},
	})
}

// GET /api/functions/{name} answers the script's source as plain text: the bug
// surfaced as `cat /bin/scan` printing the JSON-encoded record.
func getFunctionPlainSource(t *testing.T, s Subject) {
	const source = "cat /log/api.log | grep ERROR | wc -l"
	if err := s.Bins.Save(s.Owner, "", "scan", source, "bash"); err != nil {
		t.Fatal(err)
	}
	w := s.Do("GET", "/api/functions/scan", nil)
	if w.Code != 200 {
		t.Fatalf("GET function: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := strings.TrimRight(w.Body.String(), "\n")
	if body != source {
		t.Errorf("body should be the script source, got %q\nwant: %q", body, source)
	}
	if strings.HasPrefix(body, "{") && strings.Contains(body, `"Source"`) {
		t.Errorf("body looks JSON-encoded, want plain source: %q", body)
	}
}
