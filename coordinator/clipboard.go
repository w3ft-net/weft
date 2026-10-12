package coordinator

import (
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"

	"github.com/graybeardcorp/weft/internal/timerange"
	"github.com/graybeardcorp/weft/store"
)

const clipboardUsage = "clipboard: usage: clipboard /bin/<name> [--range=<spec>] [-- args...]"

// defaultShareBase is the public origin short links point at when the
// coordinator isn't configured with one.
const defaultShareBase = "https://w3ft.net"

// RegisterClipboardCommand registers `clipboard /bin/<name> [--range=<spec>]
// [-- args...]`: it records a short link to the script, the range and the
// args, prints the link and asks the terminal to copy it (OSC-52). The link
// is `<base>/s/<slug>`; it resolves only for members of the org.
//
// The range is the request's current window (so the recipient sees exactly
// what the sender saw) unless --range overrides it. start/end are that
// window.
func RegisterClipboardCommand(interp *bash.Interpreter, st *store.Store, orgID store.OrgID, userID, shareBase string, start, end time.Time) {
	if shareBase == "" {
		shareBase = defaultShareBase
	}
	interp.RegisterCommand("clipboard", func(args []string, _ io.Reader, stdout, stderr io.Writer) int {
		fail := func(msg string) int {
			fmt.Fprintln(stderr, "clipboard: "+msg)
			return 1
		}
		if len(args) == 0 {
			fmt.Fprintln(stderr, clipboardUsage)
			return 1
		}
		binPath := args[0]
		name, ok := strings.CutPrefix(binPath, "/bin/")
		if !ok || name == "" {
			return fail("first argument must be a /bin/<name> path")
		}

		rangeSpec := ""
		var scriptArgs []string
		for i := 1; i < len(args); i++ {
			a := args[i]
			if a == "--" {
				scriptArgs = append(scriptArgs, args[i+1:]...)
				break
			}
			if spec, ok := strings.CutPrefix(a, "--range="); ok {
				rangeSpec = spec
				continue
			}
			return fail("unknown option " + a)
		}
		if rangeSpec != "" {
			if _, _, err := timerange.ParseRange(rangeSpec); err != nil {
				return fail("invalid range: " + err.Error())
			}
		} else {
			const layout = "2006-01-02T15:04:05Z"
			rangeSpec = start.UTC().Format(layout) + ".." + end.UTC().Format(layout)
		}

		if fn, err := st.GetShellFunction(orgID, name); err != nil || fn == nil {
			return fail("no such script " + binPath)
		}
		slug, err := st.SaveSharedLink(orgID, userID, binPath, rangeSpec, strings.Join(scriptArgs, " "))
		if err != nil {
			return fail(err.Error())
		}
		link := strings.TrimRight(shareBase, "/") + "/s/" + slug
		// OSC-52 asks a clipboard-capable terminal to copy the link; the
		// printed copy is for terminals that drop the escape.
		fmt.Fprintf(stdout, "\x1b]52;c;%s\x07", base64.StdEncoding.EncodeToString([]byte(link)))
		fmt.Fprintf(stdout, "copied: %s\n", link)
		return 0
	})
}
