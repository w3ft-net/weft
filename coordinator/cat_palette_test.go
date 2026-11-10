package coordinator

import (
	"context"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/theme"
)

// TestRunLocal_CatHighlightsWithWeftPalette verifies that a bare `cat`
// of a /bin script executed server-side through runLocal highlights with
// weft's branded palette, not warp's default. This is the server half of
// the "highlighting relocates" contract: when the delegated client ships
// `cat /bin/x` to the exec backend instead of reading it locally, the
// bytes that come back must carry the same weave the client paints for a
// locally-read file (theme.Palette, also installed client-side via
// shell.ConfigureCore → core.SetPalette).
func TestRunLocal_CatHighlightsWithWeftPalette(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	if err := db.SaveShellFunction(custID, "", "greet", "echo hello", "bash"); err != nil {
		t.Fatalf("save: %v", err)
	}

	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	result, err := c.runLocal(context.Background(), "cat /bin/greet", TimeRange{}, custID, "", nil, nil)
	if err != nil {
		t.Fatalf("runLocal: %v", err)
	}
	out := drainExec(t, result).Text

	// `echo` is a Command token. weft paints commands in brand blue
	// (256-color 33); warp's default is bold white (15). Asserting the
	// weft opener is present and the default opener is absent proves the
	// server-side interpreter switched palettes.
	weftCommand := theme.Palette().Command // "\x1b[1;38;5;33m"
	if !strings.Contains(out, weftCommand) {
		t.Errorf("cat /bin/greet output missing weft Command SGR %q\ngot: %q", weftCommand, out)
	}
	if strings.Contains(out, "1;38;5;15m") {
		t.Errorf("cat /bin/greet still using warp default Command SGR (bold white)\ngot: %q", out)
	}
}
