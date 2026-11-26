package shell

import (
	"strings"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
)

// weftPrompt renders `[range] user@customer cwd $ `: lead returns the range
// segment (empty to omit), user is empty for no user, and the root directory
// shows as ~.
func weftPrompt(lead func() string, user, customer string) shellcore.PromptFunc {
	return func(c *shellcore.Shell) string {
		var b strings.Builder
		if l := lead(); l != "" {
			b.WriteString(l + " ")
		}
		if user != "" {
			b.WriteString(user)
			if customer != "" {
				b.WriteString("@" + customer)
			}
			b.WriteString(" ")
		}
		cwd := c.Cwd()
		if cwd == "" || cwd == "/" {
			cwd = "~"
		}
		b.WriteString(cwd + " $ ")
		return b.String()
	}
}
