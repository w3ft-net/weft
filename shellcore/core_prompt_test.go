package shellcore

import (
	"testing"
)

// TestShellCore_SetCustomerFlowsToPrompt: SetUser+SetCustomer must
// land in the next promptInfo() so the renderer sees them. Without
// this guard we could regress by adding a separate config struct that
// SetCustomer writes to but promptInfo() doesn't read.
func TestShellCore_SetCustomerFlowsToPrompt(t *testing.T) {
	c := &ShellCore{cwd: "/"}
	c.SetUser("alice")
	c.SetCustomer("acme")

	info := c.promptInfo()
	if info.User != "alice" {
		t.Errorf("promptInfo.User = %q, want %q", info.User, "alice")
	}
	if info.Customer != "acme" {
		t.Errorf("promptInfo.Customer = %q, want %q", info.Customer, "acme")
	}
	rendered := FormatPromptPrefix(info)
	if rendered != "alice@acme ~ $ " {
		t.Errorf("rendered prompt = %q, want %q", rendered, "alice@acme ~ $ ")
	}
}

// TestShellCore_EmptyCustomerStillRenders: setting only the user (the
// dev-fixture path with no JWT) must still produce a usable prompt.
// Pins the fallback-to-weft contract from a different angle than
// ansi_test — through the full promptInfo()→FormatPromptPrefix
// pipeline, in case some intermediate normalizer ever decides to
// strip "" customers.
func TestShellCore_EmptyCustomerStillRenders(t *testing.T) {
	c := &ShellCore{cwd: "/"}
	c.SetUser("paul")
	rendered := FormatPromptPrefix(c.promptInfo())
	if rendered != "paul@weft ~ $ " {
		t.Errorf("rendered prompt = %q, want %q", rendered, "paul@weft ~ $ ")
	}
}
