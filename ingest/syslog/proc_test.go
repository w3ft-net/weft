package syslog

import "testing"

func TestSplitProcID(t *testing.T) {
	for _, tc := range []struct {
		in, base, instance string
	}{
		{"web.1", "web", "web.1"},
		{"web.42", "web", "web.42"},
		{"router", "router", "router"},
		{"heroku-postgres", "heroku-postgres", "heroku-postgres"},
		{"scheduler.1234abcd", "scheduler.1234abcd", "scheduler.1234abcd"},
		{"", "", ""},
		{"-", "", ""},
		{"cron-12345", "cron-12345", "cron-12345"},
	} {
		base, inst := SplitProcID(tc.in)
		if base != tc.base || inst != tc.instance {
			t.Errorf("SplitProcID(%q) = (%q, %q), want (%q, %q)", tc.in, base, inst, tc.base, tc.instance)
		}
	}
}
