package shell

import "testing"

func TestAuthorization(t *testing.T) {
	for in, want := range map[string]string{
		"abc.def.ghi": "Bearer abc.def.ghi",
		"Basic dTpw":  "Basic dTpw",
		"Bearer x":    "Bearer x",
	} {
		if got := Authorization(in); got != want {
			t.Errorf("Authorization(%q) = %q, want %q", in, got, want)
		}
	}
}
