package onboarding

import "testing"

func TestCheckPipeline(t *testing.T) {
	cases := []struct {
		step     string
		pipeline string
		want     bool
	}{
		// Exact match advances.
		{"shell-ls", "ls /log", true},
		{"shell-tail", "tail /log/loom/prod/api/main.log", true},
		{"shell-grep-awk", "tail /log/loom/prod/api/main.log | grep ERROR | awk '{print $HOST, $1, $4}'", true},
		{"shell-alert", "tail /log/loom/prod/api/main.log | grep ERROR | wc -l | alert above 0", true},
		{"shell-notify", "cat /etc/notify.yaml", true},
		{"shell-cron", "ls /cron.d/", true},
		{"shell-bin-bash", "ls /bin/", true},
		{"shell-bin-lua", "vim /bin/severity", true},
		{"shell-wit", "wit log /bin/scan", true},
		{"shell-weftfunc", "weft::avg error-rate 1h", true},
		{"shell-tmp", "echo hello > /tmp/note", true},
		{"shell-dashboard-yaml", "cat /etc/dashboards/prod.yaml", true},
		{"shell-dashboard", "dashboard prod", true},

		// Surrounding whitespace is forgiven.
		{"shell-ls", "  ls /log  ", true},
		{"shell-ls", "ls /log\n", true},

		// Edits don't advance — the user has wandered off and
		// should re-paste from the tour overlay.
		{"shell-ls", "ls -la /log", false},
		{"shell-ls", "ls /log/", false},
		{"shell-tail", "tail -n 50 /log/loom/prod/api/main.log", false},
		{"shell-ls", "cat /log/foo.log", false},
		{"shell-grep-awk", "cat /log/loom/prod/api/main.log | grep ERROR", false},

		// Unknown step / empty step.
		{"shell-unknown", "ls /log", false},
		{"", "ls /log", false},
	}
	for _, tc := range cases {
		got := CheckPipeline(tc.step, tc.pipeline)
		if got != tc.want {
			t.Errorf("CheckPipeline(%q, %q) = %v, want %v", tc.step, tc.pipeline, got, tc.want)
		}
	}
}
