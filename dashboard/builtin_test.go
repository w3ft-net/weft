package dashboard

import (
	"testing"
)

func TestParseBuiltinArgs(t *testing.T) {
	for _, c := range []struct {
		args     []string
		name, er string
	}{
		{nil, "", ""},
		{[]string{"prod"}, "prod", ""},
		{[]string{"prod", "--fullscreen"}, "", "unknown option --fullscreen"},
		{[]string{"-x"}, "", "unknown option -x"},
		{[]string{"prod", "staging"}, "", "unexpected argument staging"},
	} {
		name, er := parseBuiltinArgs(c.args)
		if name != c.name || er != c.er {
			t.Errorf("parseBuiltinArgs(%v) = %q, %q; want %q, %q", c.args, name, er, c.name, c.er)
		}
	}
}
