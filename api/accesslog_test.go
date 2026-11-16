package api

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/w3ft-net/weft/logtest"
)

// AccessLog read the session off the parent request context, but the identity
// middleware injects it into a child context that flows downward only, so every
// line logged user=anon whether the caller was authenticated or not, which made
// the access log useless for telling a real auth bypass from a logging artifact.
// An authenticated request is attributed to its user; an anonymous one still
// reads user=anon.
func TestAccessLogAttributesUser(t *testing.T) {
	a := newOpenAPI(t)
	var buf bytes.Buffer
	t.Cleanup(logtest.CaptureInto(&buf))

	a.do("GET", "/api/functions", nil)
	if line := buf.String(); !strings.Contains(line, "user="+openUser) || strings.Contains(line, "user=anon") {
		t.Errorf("authenticated request not attributed: want user=%s in the access log, got:\n%s", openUser, line)
	}

	buf.Reset()
	a.h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/functions", nil))
	if line := buf.String(); !strings.Contains(line, "user=anon") {
		t.Errorf("unauthenticated request should log user=anon, got:\n%s", line)
	}
}
