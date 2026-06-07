package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/internal/clientwire"
)

// TestAccessLog_PreservesFlusher pins the regression introduced when
// AccessLog began wrapping the whole mux: its statusRecorder embedded
// http.ResponseWriter but did not forward Flush(), so the SSE shell
// handler's `w.(http.Flusher)` assertion failed and every streaming
// request 500'd with "streaming unsupported". httptest.ResponseRecorder
// is itself a Flusher, so a correctly transparent wrapper must keep the
// downstream writer Flusher-capable.
func TestAccessLog_PreservesFlusher(t *testing.T) {
	var gotFlusher bool
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, gotFlusher = w.(http.Flusher)
	})
	rec := httptest.NewRecorder()
	if _, ok := http.ResponseWriter(rec).(http.Flusher); !ok {
		t.Fatal("precondition: httptest.ResponseRecorder should be an http.Flusher")
	}
	AccessLog(terminal).ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if !gotFlusher {
		t.Fatal("AccessLog dropped http.Flusher: SSE/streaming handlers will 500")
	}
}

// nonFlusherWriter is a minimal http.ResponseWriter that is deliberately
// NOT an http.Flusher.
type nonFlusherWriter struct{ h http.Header }

func (n *nonFlusherWriter) Header() http.Header         { return n.h }
func (n *nonFlusherWriter) Write(b []byte) (int, error) { return len(b), nil }
func (n *nonFlusherWriter) WriteHeader(int)             {}

// flushSpy records whether Flush() actually reached it.
type flushSpy struct {
	*nonFlusherWriter
	flushed bool
}

func (f *flushSpy) Flush() { f.flushed = true }

// unwrapToFlusher is NOT itself an http.Flusher; it exposes a flushable
// writer only via Unwrap() — exactly the shape of a middleware-wrapped
// or HTTP/2 server ResponseWriter under the http.ResponseController
// convention (Go 1.20+).
type unwrapToFlusher struct {
	*nonFlusherWriter
	inner *flushSpy
}

func (u *unwrapToFlusher) Unwrap() http.ResponseWriter { return u.inner }

// TestAccessLog_FlushReachesWriter pins the *delivery* half of the
// streaming contract that TestAccessLog_PreservesFlusher misses. It is
// not enough that w.(http.Flusher) is satisfied — it is, because
// statusRecorder has a Flush method — calling that Flush must actually
// reach the underlying writer.
//
// The shipped 4df18c4 implementation forwards via a one-level
// `s.ResponseWriter.(http.Flusher)` assertion, which silently no-ops
// whenever the wrapped writer exposes flushing only through Unwrap()
// (every non-trivial wrapper and the HTTP/2 server writer). In prod
// that means w.(http.Flusher) succeeds so the SSE handler does NOT 500
// (no HTTP status in DevTools), but no bytes are ever flushed: the
// browser's streaming fetch stalls, the connection resets, and the
// WASM shell prints "cannot reach API server" for every log query
// while non-streaming endpoints are unaffected.
func TestAccessLog_FlushReachesWriter(t *testing.T) {
	spy := &flushSpy{nonFlusherWriter: &nonFlusherWriter{h: http.Header{}}}
	base := &unwrapToFlusher{nonFlusherWriter: &nonFlusherWriter{h: http.Header{}}, inner: spy}

	// Precondition: AccessLog wraps a writer that is NOT a direct
	// Flusher, flushable only through Unwrap() — the realistic shape.
	if _, ok := http.ResponseWriter(base).(http.Flusher); ok {
		t.Fatal("precondition: base writer must not be a direct http.Flusher")
	}

	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher) // exactly what api/shell.go:84 does
		if !ok {
			t.Fatal("w.(http.Flusher) failed: SSE handler would 500")
		}
		f.Flush()
	})

	AccessLog(terminal).ServeHTTP(base, httptest.NewRequest("GET", "/x", nil))

	if !spy.flushed {
		t.Fatal("Flush() through AccessLog's statusRecorder never reached the " +
			"underlying writer: SSE/streaming silently stalls in prod even though " +
			"w.(http.Flusher) is satisfied")
	}
}

// TestCORS_AllowsActiveOrgHeader pins the regression where the welcome
// modal's Start-the-tour click fired a cross-origin /api/auth/me with
// the X-Active-Org header set to "loom" (from switchOrg's localStorage
// write + reload). The browser's preflight to api.w3ft.net came back
// with Access-Control-Allow-Headers: Authorization, Content-Type —
// X-Active-Org was missing, so the actual request never went out, the
// frontend's getMe rejected, and AuthContext cleared the token and
// bounced the user to /login.
//
// Every cross-origin authed call uses this header (api.ts:38 sets it
// from localStorage on every request), so the gap affected more than
// the tour — anyone who'd ever switched orgs would hit it on the next
// page reload. The fix is just to include the header in the preflight
// response.
func TestCORS_AllowsActiveOrgHeader(t *testing.T) {
	handler := CORSMiddleware("https://w3ft.net", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodOptions, "/api/auth/me", nil)
	req.Header.Set("Origin", "https://w3ft.net")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "x-active-org")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight: expected 204, got %d", w.Code)
	}
	allow := w.Header().Get("Access-Control-Allow-Headers")
	if !strings.Contains(strings.ToLower(allow), "x-active-org") {
		t.Fatalf("preflight Access-Control-Allow-Headers must include X-Active-Org (got %q); without it the browser blocks every cross-origin authed call once the SPA has an org pinned",
			allow)
	}
}

// TestCORS_AllowsAllClientWireHeaders is the rot-proof guard for this
// whole bug class. The X-Active-Org test above and the X-Weft-Client
// incident were the SAME defect twice: a header was added to the shared
// shell client but not to CORSMiddleware's Access-Control-Allow-Headers,
// so the browser shell died at preflight while the CLI (which does no
// preflight) masked it.
//
// No Go test driving the server through the real client can catch this
// — net/http ignores CORS; only a browser enforces it. So the contract
// is asserted directly against clientwire.AuthHeaders, the single slice
// both the client (shell.setAuthHeaders) and the server (CORSMiddleware)
// derive from. A header added there must surface in the preflight
// response for every entry, or this fails — and because CORSMiddleware
// builds the value from the same slice, the two cannot drift. The test
// also pins the preflight 204 and the browser-equivalent behavior
// (request named in Access-Control-Request-Headers).
func TestCORS_AllowsAllClientWireHeaders(t *testing.T) {
	handler := CORSMiddleware("https://w3ft.net", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodOptions, "/api/shell/exec", nil)
	req.Header.Set("Origin", "https://w3ft.net")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", strings.ToLower(strings.Join(clientwire.AuthHeaders, ", ")))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight: expected 204, got %d", w.Code)
	}
	allow := strings.ToLower(w.Header().Get("Access-Control-Allow-Headers"))
	for _, name := range clientwire.AuthHeaders {
		if !strings.Contains(allow, strings.ToLower(name)) {
			t.Fatalf("the shared client sends %s on cross-origin requests (clientwire.AuthHeaders) "+
				"but CORSMiddleware Access-Control-Allow-Headers=%q does not list it; the browser "+
				"shell would block every call at preflight and report \"cannot reach API server\" "+
				"while the CLI keeps working.",
				name, w.Header().Get("Access-Control-Allow-Headers"))
		}
	}
}
