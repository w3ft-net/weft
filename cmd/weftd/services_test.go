package main

import "testing"

func TestPrefixNamespace(t *testing.T) {
	cases := []struct {
		namespace string
		app       string
		want      string
	}{
		{"weft", "coordinator/runtime", "weft/coordinator/runtime"},
		{"weft", "weft/coordinator/runtime", "weft/coordinator/runtime"}, // already qualified
		{"weft", "weft", "weft"},                                          // single-segment match
		{"", "anything", "anything"},                                      // no namespace configured
		{"weft", "vimgym/leak", "weft/vimgym/leak"},                       // cross-namespace SDK input still gets weftd's ns stamped
	}
	for _, tc := range cases {
		got := prefixNamespace(tc.namespace, tc.app)
		if got != tc.want {
			t.Errorf("prefixNamespace(%q,%q) = %q, want %q", tc.namespace, tc.app, got, tc.want)
		}
	}
}

func TestEffectiveServices(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want []string
	}{
		{"explicit list wins", Config{Services: []string{"coordinator", "api"}, Deployment: "coordinator"}, []string{"coordinator", "api"}},
		{"falls back to deployment", Config{Deployment: "shard"}, []string{"shard"}},
		{"empty when neither set", Config{}, nil},
	}
	for _, tc := range cases {
		got := effectiveServices(tc.cfg)
		if len(got) != len(tc.want) {
			t.Errorf("%s: len = %d, want %d", tc.name, len(got), len(tc.want))
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: [%d] = %q, want %q", tc.name, i, got[i], tc.want[i])
			}
		}
	}
}
