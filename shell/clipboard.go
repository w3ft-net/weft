package shell

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/internal/timerange"
)

// clipboardBuiltin handles `clipboard /bin/<name> [--range=<spec>] [-- args...]`.
//
// It builds a deep-link URL pointing at the named /bin/ script with the
// current shell range (or an explicit override) and any positional args
// after `--`, then emits OSC-52 so a clipboard-capable terminal copies
// the URL. The URL is also appended to scrollback so the user can grab
// it manually if their terminal doesn't honor OSC-52.
//
// No store lookup happens here — the builtin is purely string
// manipulation, which keeps it network-free and identical between CLI
// and web shells. The reader (Shell.tsx) resolves the script at HEAD
// when the link is opened.
func clipboardBuiltin(s *Shell, shareBase string) shellcore.BuiltinFunc {
	return func(c *shellcore.ShellCore, args []string) bool {
		if len(args) == 0 || args[0] != "clipboard" {
			return false
		}
		if len(args) < 2 {
			c.AppendScrollback(shellcore.ErrorText("clipboard: usage: clipboard /bin/<name> [--range=<spec>] [-- args...]") + "\n")
			return true
		}

		binPath := args[1]
		if !strings.HasPrefix(binPath, "/bin/") || binPath == "/bin/" {
			c.AppendScrollback(shellcore.ErrorText("clipboard: first argument must be a /bin/<name> path") + "\n")
			return true
		}

		rangeSpec := ""
		var scriptArgs []string
		for i := 2; i < len(args); i++ {
			a := args[i]
			if a == "--" {
				scriptArgs = append(scriptArgs, args[i+1:]...)
				break
			}
			if strings.HasPrefix(a, "--range=") {
				rangeSpec = strings.TrimPrefix(a, "--range=")
				continue
			}
			c.AppendScrollback(shellcore.ErrorText("clipboard: unknown option "+a) + "\n")
			return true
		}

		if rangeSpec == "" {
			// Resolve s.Range fresh at copy time so the deep link pins a
			// concrete window (the recipient's clock is irrelevant — they
			// should see exactly what the sender saw). The prompt's lead
			// segment is a display-only snapshot from the last `range`
			// change and may be hours stale; ParseRange anchors to now.
			start, end, err := timerange.ParseRange(s.Range)
			if err != nil {
				c.AppendScrollback(shellcore.ErrorText("clipboard: invalid current range: "+err.Error()) + "\n")
				return true
			}
			rangeSpec = start.UTC().Format("2006-01-02T15:04:05Z") + ".." + end.UTC().Format("2006-01-02T15:04:05Z")
		}

		link := buildClipboardURL(shareBase, binPath, rangeSpec, scriptArgs)
		c.AppendScrollback(emitOSC52(link))
		c.AppendScrollback(shellcore.DimText("copied: ") + link + "\n")
		return true
	}
}

// buildClipboardURL assembles the shell deep-link URL. `bin` and
// `range` are unconditional; `args` appears only when the caller
// passed positional args after `--`.
func buildClipboardURL(base, binPath, rangeSpec string, scriptArgs []string) string {
	q := url.Values{}
	q.Set("bin", binPath)
	q.Set("range", rangeSpec)
	if len(scriptArgs) > 0 {
		q.Set("args", strings.Join(scriptArgs, " "))
	}
	return strings.TrimRight(base, "/") + "/shell?" + q.Encode()
}

// emitOSC52 returns the terminal escape that asks the host emulator
// to write `payload` to the system clipboard. The "c" selector is the
// clipboard (vs. "p" for primary). Terminals that don't recognize
// OSC-52 silently drop the sequence — the caller still prints the
// payload to scrollback so the user can copy it manually.
func emitOSC52(payload string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(payload))
	return fmt.Sprintf("\x1b]52;c;%s\x07", enc)
}
