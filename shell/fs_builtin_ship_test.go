package shell

import (
	"bytes"
	"testing"
)

// `ls` and `cat` on FS paths ship to the server like any other line.
// The one-shot path used to answer them itself from the first word of the
// line, which dropped everything after the first path: `ls /etc | wc -l`
// printed the listing instead of a count, `cat /etc/passwd | wc -l` the
// file, `ls /etc /bin` only /etc, and `ls /etc/notify.yaml` failed with
// "file does not exist" because it read a file as a directory.
func TestExecNonInteractiveFSLinesShipVerbatim(t *testing.T) {
	for _, line := range []string{
		"ls /etc/notify.yaml",
		"ls /etc | wc -l",
		"ls /etc /bin",
		"ls -a /etc",
		"cat /etc/passwd | wc -l",
		"cat /bin/scan",
	} {
		s, captured := newCaptureShell()
		var buf bytes.Buffer
		if err := s.ExecNonInteractive(line, &buf); err != nil {
			t.Fatalf("ExecNonInteractive(%q): %v", line, err)
		}
		if *captured != line {
			t.Errorf("%q: Query received %q, want it shipped verbatim", line, *captured)
		}
	}
}
