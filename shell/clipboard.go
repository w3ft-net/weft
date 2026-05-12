package shell

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"

	"github.com/kevlarcon/warp/pkg/shellcore"
)

// clipboardBuiltin handles `clipboard /bin/<name>[@v] [--range=<spec>] [-- args...]`.
//
// It builds a deep-link URL pointing at the named /bin/ script with the
// current shell range (or an explicit override) and any positional args
// after `--`, then emits OSC-52 so a clipboard-capable terminal copies
// the URL. The URL is also appended to scrollback so the user can grab
// it manually if their terminal doesn't honor OSC-52.
//
// Version pinning (`@N`) is recorded in the URL but the reader treats
// unset = HEAD; no store lookup happens here, which keeps the builtin
// network-free and lets it work identically in CLI and web shells.
func clipboardBuiltin(shareBase string) shellcore.BuiltinFunc {
	return func(c *shellcore.ShellCore, args []string) bool {
		if len(args) == 0 || args[0] != "clipboard" {
			return false
		}
		if len(args) < 2 {
			c.AppendScrollback(shellcore.ErrorText("clipboard: usage: clipboard /bin/<name>[@version] [--range=<spec>] [-- args...]") + "\n")
			return true
		}

		binPath, version, ok := parseBinRef(args[1])
		if !ok {
			c.AppendScrollback(shellcore.ErrorText("clipboard: first argument must be a /bin/<name> path") + "\n")
			return true
		}

		rangeSpec := ""
		var scriptArgs []string
		i := 2
		for ; i < len(args); i++ {
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
			start, end := c.Range()
			rangeSpec = start.UTC().Format("2006-01-02T15:04:05Z") + ".." + end.UTC().Format("2006-01-02T15:04:05Z")
		}

		link := buildClipboardURL(shareBase, binPath, version, rangeSpec, scriptArgs)
		c.AppendScrollback(emitOSC52(link))
		c.AppendScrollback(shellcore.DimText("copied: ") + link + "\n")
		return true
	}
}

// parseBinRef splits a `/bin/<name>` or `/bin/<name>@<version>` ref.
// Returns (path-without-version, version-or-empty, ok). ok is false
// when the ref isn't a /bin/ path.
func parseBinRef(ref string) (string, string, bool) {
	if !strings.HasPrefix(ref, "/bin/") {
		return "", "", false
	}
	if at := strings.LastIndex(ref, "@"); at > len("/bin/") {
		return ref[:at], ref[at+1:], true
	}
	return ref, "", true
}

// buildClipboardURL assembles the shell deep-link URL. Only `bin` and
// `range` are unconditional; `v` and `args` appear only when set.
func buildClipboardURL(base, binPath, version, rangeSpec string, scriptArgs []string) string {
	q := url.Values{}
	q.Set("bin", binPath)
	if version != "" {
		q.Set("v", version)
	}
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
