package main

import "testing"

func TestConfigError(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		expect string
	}{
		{"usable config", Config{Token: "wft_x"}, ""},
		{"missing token", Config{}, "token is required: set token: in the config file or WEFTD_TOKEN in the environment"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := configError(c.cfg); got != c.expect {
				t.Fatalf("configError() = %q, want %q", got, c.expect)
			}
		})
	}
}
