package shell

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// TestEmailLocalPart pins the substitution that drives the prompt's
// user portion: alice@acme.test → "alice". Edge cases (no @, empty
// string, multiple @s) are explicit so the prompt never renders
// something embarrassing.
func TestEmailLocalPart(t *testing.T) {
	cases := []struct{ in, want string }{
		{"alice@acme.test", "alice"},
		{"dana@globex.test", "dana"},
		{"noatsign", "noatsign"},
		{"", ""},
		// Two @s: take the part before the first one.
		{"weird@two@example.com", "weird"},
	}
	for _, c := range cases {
		if got := EmailLocalPart(c.in); got != c.want {
			t.Errorf("EmailLocalPart(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDecodeClaims_RoundTrip builds a fake JWT (header.payload.sig)
// with known claims and confirms DecodeClaims pulls the user ID and
// expiry out unchanged. We don't validate the signature in this path —
// that's the api server's job — so any well-formed JWT shape works.
// Identity fields beyond `sub` aren't on the JWT anymore; the CLI
// fetches them from /api/auth/me when it needs them.
func TestDecodeClaims_RoundTrip(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"sub": "user-123",
		"iat": 1700000000,
		"exp": 1900000000,
	})
	tok := strings.Join([]string{
		base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`)),
		base64.RawURLEncoding.EncodeToString(payload),
		base64.RawURLEncoding.EncodeToString([]byte("fake-signature")),
	}, ".")

	claims, err := DecodeClaims(tok)
	if err != nil {
		t.Fatalf("DecodeClaims: %v", err)
	}
	if claims.UserID != "user-123" {
		t.Errorf("UserID = %q, want user-123", claims.UserID)
	}
	if claims.Expires != 1900000000 {
		t.Errorf("Expires = %d, want 1900000000", claims.Expires)
	}
}

// TestDecodeClaims_RejectsMalformed: a token that isn't three dot-
// separated segments is invalid; we should error rather than silently
// produce a zero-valued struct.
func TestDecodeClaims_RejectsMalformed(t *testing.T) {
	if _, err := DecodeClaims("not-a-jwt"); err == nil {
		t.Error("expected error for non-JWT string")
	}
	if _, err := DecodeClaims(""); err == nil {
		t.Error("expected error for empty string")
	}
}

// TestDecodeClaims_RejectsBadPayload: a JWT-shaped string with a
// payload that isn't valid base64 should error rather than panic.
func TestDecodeClaims_RejectsBadPayload(t *testing.T) {
	if _, err := DecodeClaims("aaa.&&&.bbb"); err == nil {
		t.Error("expected error for invalid base64 payload")
	}
}
