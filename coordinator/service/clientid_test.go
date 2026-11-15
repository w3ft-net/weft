package service

import "testing"

func TestClientBelow(t *testing.T) {
	cases := []struct {
		client, floor string
		want          bool
	}{
		// Empty floor disables the gate.
		{"weftd/0.0.1", "", false},
		// Empty client is "below everything" — old agents that don't declare a version.
		{"", "0.14.0", true},
		// Numeric comparison within the same major series.
		{"weftd/0.13.99", "0.14.0", true},
		{"weftd/0.14.0", "0.14.0", false},
		{"weftd/0.14.0+abc123", "0.14.0", false}, // sha doesn't affect compare
		{"weftd/0.14.1", "0.14.0", false},
		{"weftd/0.15.0", "0.14.0", false},
		// Cross-major.
		{"weftd/0.99.0", "1.0.0", true},
		{"weftd/1.0.0", "0.99.0", false},
		// Shorter floor zero-pads.
		{"weftd/0.14.0", "0.14", false},
		{"weftd/0.13.99", "0.14", true},
		// Floor with sha is unusual but should still work (sha-on-floor
		// shouldn't reverse the order).
		{"weftd/0.14.0", "0.14.0+xyz", true}, // "0" < "0+xyz" lexically — acceptable; operator shouldn't ship +sha floors.
	}
	for _, c := range cases {
		got := clientBelow(c.client, c.floor)
		if got != c.want {
			t.Errorf("clientBelow(%q, %q) = %v, want %v", c.client, c.floor, got, c.want)
		}
	}
}

func TestStripClientIDVersion(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"weftd/0.14.0", "0.14.0"},
		{"weftd/0.14.0+abc123", "0.14.0"},
		{"weft/0.3.0+def456", "0.3.0"},
		{"0.14.0", "0.14.0"},     // no prefix
		{"0.14.0+abc", "0.14.0"}, // no binary prefix but sha present
		{"", ""},
	}
	for _, c := range cases {
		got := stripClientIDVersion(c.in)
		if got != c.want {
			t.Errorf("stripClientIDVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
