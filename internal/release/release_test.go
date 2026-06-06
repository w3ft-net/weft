package release

import "testing"

func TestVersionNewer(t *testing.T) {
	cases := []struct {
		candidate, current string
		want               bool
	}{
		{"0.19.0", "0.18.0", true},           // patch/minor forward
		{"0.18.0", "0.18.0", false},          // equal is not newer
		{"0.17.0", "0.18.0", false},          // older
		{"0.18.0", "0.17.0", true},           // the staging hop
		{"0.19.0", "0.18.0", true},           // the heal hop
		{"1.0.0", "0.99.0", true},            // major beats larger minor
		{"0.18", "0.18.0", false},            // zero-padded equal
		{"0.18.1", "0.18", true},             // zero-padded forward
		{"0.100.0", "0.99.0", true},          // numeric, not lexicographic (100 > 99)
		{"weftd/0.19.0+abc", "0.18.0", true}, // raw ClientID prefix/suffix stripped
		{"0.18.0", "weftd/0.18.0+def", false},
	}
	for _, c := range cases {
		if got := VersionNewer(c.candidate, c.current); got != c.want {
			t.Errorf("VersionNewer(%q, %q) = %v, want %v", c.candidate, c.current, got, c.want)
		}
	}
}
