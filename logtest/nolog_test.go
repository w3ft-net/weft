package logtest

import "testing"

// The operator tools of this module that print to a terminal.
var terminalTools = []string{"cmd/weft", "bench", "integration", "docgen", "scripts"}

// TestNoStdlibLogPrintsOutsideTerminalTools keeps trace IDs automatic in this
// module: only the SDK (ctx-aware, through its slog handler) may write a server
// log line. The hosted tree runs the same scan over itself.
func TestNoStdlibLogPrintsOutsideTerminalTools(t *testing.T) {
	found, err := StdlibLogPrints("..", terminalTools...)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Error(FormatStdlibLogPrints(found))
	}
}
