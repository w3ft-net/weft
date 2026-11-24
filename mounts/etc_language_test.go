package mounts

import (
	"context"
	"testing"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
)

// TestEtcMount_FileLanguage pins that /etc's YAML files self-identify so
// `cat` highlights them. Without a LangMount implementation the shell
// falls back to bash (client) or plain text (server-side cat), which is
// what the onboarding tour's `cat /etc/notify.yaml` lesson showed.
// passwd and usage are not YAML and must stay unhighlighted.
func TestEtcMount_FileLanguage(t *testing.T) {
	var m shellcore.Mount = &EtcMount{}
	lm, ok := m.(shellcore.LangMount)
	if !ok {
		t.Fatalf("EtcMount does not implement shellcore.LangMount")
	}
	cases := map[string]string{
		"notify.yaml":          "yaml",
		"dashboards/prod.yaml": "yaml",
		"passwd":               "",
		"usage":                "",
	}
	for rel, want := range cases {
		if got := lm.FileLanguage(context.Background(), rel); got != want {
			t.Errorf("FileLanguage(%q) = %q, want %q", rel, got, want)
		}
	}
}
