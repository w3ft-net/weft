// Package clientwire holds the canonical set of request headers a weft
// browser client sends, so the api server's CORS layer can be built
// from it instead of a hand-maintained string literal.
//
// It exists to make one specific bug structurally impossible on the
// server side: a request header the browser sends that is missing from
// Access-Control-Allow-Headers. The browser blocks such a request at
// CORS preflight before it is ever sent, so it never reaches the server
// (no access-log line) and the web shell reports "cannot reach API
// server". The CLI does no preflight, so it (and coordinator crons)
// keep working — which is why this looks like an auth/CORS puzzle
// instead of a missing allow-list entry. It regressed once for
// X-Active-Org and again for X-Weft-Client.
//
// The browser client is TypeScript (frontend/src/wasm/network.ts +
// frontend/src/lib/api.ts); this slice is the Go-side mirror of the
// headers those send. api.CORSMiddleware builds
// Access-Control-Allow-Headers from this exact slice, and
// api.TestCORS_AllowsAllClientWireHeaders pins the contract — so the
// server can never drift from this list. Keeping this list in sync with
// the TS clients is the one remaining manual step; see memory
// feedback_cors_shared_client_headers.
package clientwire

// AuthHeaders is the exhaustive set of request headers a browser client
// attaches to a cross-origin request, in the order they should appear
// in Access-Control-Allow-Headers. Content-Type is included because the
// clients send application/json bodies (not a CORS-safelisted value, so
// it triggers preflight); the rest are the auth/identity headers set on
// every request (see frontend/src/wasm/network.ts authHeaders()).
var AuthHeaders = []string{
	"Authorization",
	"Content-Type",
	"X-Active-Org",
	"X-Weft-Client",
}
