package mounts

import (
	"strings"
	"testing"

	"github.com/w3ft-net/weft/cronauthor"
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
			_, _, enabled, err := cronauthor.Parse(tc.content)
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
