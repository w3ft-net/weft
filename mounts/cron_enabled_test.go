package mounts

import (
	"fmt"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/store"
)

func TestParseCron_Enabled(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		want          bool
		wantErr       string
	}{
		{"no comment means enabled", "# schedule: every 5m\ntrue\n", true, ""},
		{"explicit true", "# schedule: every 5m\n# enabled: true\ntrue\n", true, ""},
		{"explicit false", "# schedule: every 5m\n# enabled: false\ntrue\n", false, ""},
		{"garbage value", "# schedule: every 5m\n# enabled: maybe\ntrue\n", false, "enabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, enabled, err := ParseCron(tc.content)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one mentioning %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if enabled != tc.want {
				t.Errorf("enabled = %v, want %v", enabled, tc.want)
			}
		})
	}
}

// A disabled cron round-trips through its own file: reading it back and
// writing it again must not re-enable it.
func TestCronMount_DisabledRoundTrip(t *testing.T) {
	cfg := ownerCfg(t, []string{"x/y"})
	cfg.Tier = store.TierSwatch
	m := &CronMount{cfg: cfg}

	if err := m.WriteFile("paused", []byte("# schedule: every 5m\n# enabled: false\ntrue\n")); err != nil {
		t.Fatal(err)
	}
	data, err := m.ReadFile("paused")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# enabled: false") {
		t.Fatalf("file lost its disabled marker:\n%s", data)
	}
	if err := m.WriteFile("paused", data); err != nil {
		t.Fatal(err)
	}
	w, err := cfg.Store.GetCronWatch(cfg.OrgID, "paused")
	if err != nil {
		t.Fatal(err)
	}
	if w.Enabled {
		t.Error("rewriting the file as read re-enabled the cron")
	}

	// Dropping the comment is how a user turns it back on.
	if err := m.WriteFile("paused", []byte("# schedule: every 5m\ntrue\n")); err != nil {
		t.Fatal(err)
	}
	if w, _ = cfg.Store.GetCronWatch(cfg.OrgID, "paused"); !w.Enabled {
		t.Error("saving without the comment did not re-enable the cron")
	}
}

// Disabled crons are free: they don't count against the plan, and turning
// one back on is refused when the org is full.
func TestCronMount_DisabledCronsDontCountAgainstLimit(t *testing.T) {
	cfg := ownerCfg(t, []string{"x/y"})
	cfg.Tier = store.TierSwatch
	m := &CronMount{cfg: cfg}
	limit := store.WatchLimit(cfg.Tier)

	for i := 0; i < limit; i++ {
		if err := m.WriteFile(fmt.Sprintf("c%d", i), []byte("# schedule: every 5m\ntrue\n")); err != nil {
			t.Fatalf("cron %d: %v", i, err)
		}
	}
	if err := m.WriteFile("extra", []byte("# schedule: every 5m\ntrue\n")); err == nil {
		t.Fatal("cron past the limit was accepted")
	}
	if err := m.WriteFile("extra", []byte("# schedule: every 5m\n# enabled: false\ntrue\n")); err != nil {
		t.Fatalf("a disabled cron counted against the limit: %v", err)
	}
	err := m.WriteFile("extra", []byte("# schedule: every 5m\ntrue\n"))
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("re-enabling at the limit: err = %v, want a limit error", err)
	}
	// Freeing a slot lets it back on.
	if err := m.WriteFile("c0", []byte("# schedule: every 5m\n# enabled: false\ntrue\n")); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteFile("extra", []byte("# schedule: every 5m\ntrue\n")); err != nil {
		t.Fatalf("re-enabling with a free slot: %v", err)
	}
}
