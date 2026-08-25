package main

import "testing"

// configError names the reason a parsed config cannot be run, or "" when
// it is usable. Split out of main() so the startup decision below is
// testable without driving os.Exit.
func TestConfigError(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		expect string
	}{
		{
			name:   "usable config",
			cfg:    Config{Endpoint: "ingest.w3ft.net:9020", Token: "wft_x"},
			expect: "",
		},
		{
			name:   "missing token",
			cfg:    Config{Endpoint: "ingest.w3ft.net:9020"},
			expect: "token is required: set token: in the config file or WEFTD_TOKEN in the environment",
		},
		{
			name:   "missing endpoint",
			cfg:    Config{Token: "wft_x"},
			expect: "endpoint is required",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := configError(c.cfg); got != c.expect {
				t.Fatalf("configError() = %q, want %q", got, c.expect)
			}
		})
	}
}

// A usable config just runs; self-update is never consulted on the
// startup path (it still runs in the background from main).
func TestDecideStartupRunsUsableConfig(t *testing.T) {
	rescued := false
	cfg := Config{Endpoint: "ingest.w3ft.net:9020", Token: "wft_x"}

	got, reason := decideStartup(cfg, func() bool { rescued = true; return true })

	if got != startupRun {
		t.Fatalf("decideStartup() = %v, want startupRun", got)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want empty", reason)
	}
	if rescued {
		t.Fatal("self-update consulted for a config that is already usable")
	}
}

// The xylem regression. A config authored for a newer weftd (no
// endpoint:, which 0.23.0 defaults and 0.20.0 rejects) must not be a
// terminal state for an older binary: config validation cannot be the
// thing that gates the self-update that would fix it. Before this,
// main() fataled on the missing key ~15 lines before it ever started
// the SelfUpdateChecker, so the agent could never climb to the version
// that understood its own config — it crash-looped to the unit's
// StartLimitBurst cap and shipped nothing until an operator intervened.
func TestDecideStartupRescuesConfigFromNewerWeftd(t *testing.T) {
	cfg := Config{Token: "wft_x"} // no endpoint: — correct for >= 0.23.0

	got, reason := decideStartup(cfg, func() bool { return true })

	if got != startupRescued {
		t.Fatalf("decideStartup() = %v, want startupRescued", got)
	}
	if reason != "endpoint is required" {
		t.Fatalf("reason = %q, want the underlying config error", reason)
	}
}

// Self-update is a rescue, not a cure-all: when no newer release is
// available the config error is still fatal, and the operator sees the
// real reason rather than a silent restart loop.
func TestDecideStartupFatalsWhenNoUpdateAvailable(t *testing.T) {
	cfg := Config{Token: "wft_x"}

	got, reason := decideStartup(cfg, func() bool { return false })

	if got != startupFatal {
		t.Fatalf("decideStartup() = %v, want startupFatal", got)
	}
	if reason != "endpoint is required" {
		t.Fatalf("reason = %q, want the underlying config error", reason)
	}
}

// A host pinned with auto_update: false gets the fatal it asked for. We
// do not climb versions behind an operator's back, even to rescue a
// config this binary cannot parse — pinning is usually how someone
// investigates a bad release.
func TestDecideStartupHonorsAutoUpdateOptOut(t *testing.T) {
	off := false
	cfg := Config{Token: "wft_x", AutoUpdate: &off}
	consulted := false

	got, _ := decideStartup(cfg, func() bool { consulted = true; return true })

	if got != startupFatal {
		t.Fatalf("decideStartup() = %v, want startupFatal", got)
	}
	if consulted {
		t.Fatal("self-update attempted on a host pinned with auto_update: false")
	}
}
