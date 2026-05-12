package shell

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestBuildClipboardURL(t *testing.T) {
	cases := []struct {
		name     string
		base     string
		path     string
		rng      string
		args     []string
		wantHost string
		wantHave []string
		wantMiss []string
	}{
		{
			name:     "minimal",
			base:     "https://w3ft.net",
			path:     "/bin/error-rate",
			rng:      "2026-05-12T00:00:00Z..2026-05-12T01:00:00Z",
			wantHost: "https://w3ft.net/shell?",
			wantHave: []string{"bin=%2Fbin%2Ferror-rate", "range=2026-05-12T00%3A00%3A00Z..2026-05-12T01%3A00%3A00Z"},
			wantMiss: []string{"v=", "args="},
		},
		{
			name:     "with args",
			base:     "https://w3ft.net/",
			path:     "/bin/error-rate",
			rng:      "24h..",
			args:     []string{"--app=loom", "--since=1h"},
			wantHost: "https://w3ft.net/shell?",
			wantHave: []string{"bin=%2Fbin%2Ferror-rate", "range=24h..", "args=--app%3Dloom+--since%3D1h"},
		},
		{
			name:     "trailing slash on base is normalized",
			base:     "http://localhost:5173/",
			path:     "/bin/foo",
			rng:      "1h..",
			wantHost: "http://localhost:5173/shell?",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildClipboardURL(c.base, c.path, c.rng, c.args)
			if !strings.HasPrefix(got, c.wantHost) {
				t.Fatalf("missing host prefix\n  got:  %s\n  want prefix: %s", got, c.wantHost)
			}
			for _, frag := range c.wantHave {
				if !strings.Contains(got, frag) {
					t.Errorf("missing fragment %q in %s", frag, got)
				}
			}
			for _, frag := range c.wantMiss {
				if strings.Contains(got, frag) {
					t.Errorf("unexpected fragment %q in %s", frag, got)
				}
			}
		})
	}
}

func TestEmitOSC52(t *testing.T) {
	payload := "https://w3ft.net/shell?bin=/bin/foo&range=1h.."
	got := emitOSC52(payload)
	if !strings.HasPrefix(got, "\x1b]52;c;") || !strings.HasSuffix(got, "\x07") {
		t.Fatalf("OSC-52 frame malformed: %q", got)
	}
	encoded := strings.TrimSuffix(strings.TrimPrefix(got, "\x1b]52;c;"), "\x07")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("payload not valid base64: %v", err)
	}
	if string(decoded) != payload {
		t.Errorf("decoded payload mismatch\n  got:  %q\n  want: %q", string(decoded), payload)
	}
}
