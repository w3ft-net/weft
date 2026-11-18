package mounts

import (
	"context"
	"testing"

	"github.com/graybeardcorp/warp/pkg/shellcore"
)

// TestCronMount_FileLanguage pins that /cron.d entries self-identify as
// bash (a `# schedule:` comment header plus a bash pipeline). The
// coordinator's server-side cat has no syntax fallback and relies on the
// mount's answer; without it `cat /cron.d/x` came back uncolored.
func TestCronMount_FileLanguage(t *testing.T) {
	var m shellcore.Mount = &CronMount{}
	lm, ok := m.(shellcore.LangMount)
	if !ok {
		t.Fatalf("CronMount does not implement shellcore.LangMount")
	}
	if got := lm.FileLanguage(context.Background(), "nightly-errors"); got != "bash" {
		t.Errorf("FileLanguage = %q, want bash", got)
	}
}
