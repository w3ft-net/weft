package shellcore

import "testing"

// TestFormatPromptPrefix_LoggedIn pins the headline behavior we just
// fixed: a logged-in user sees `alice@acme ~ $`, not the legacy
// hardcoded `@weft` slug. ShellCore.SetCustomer + the new
// PromptInfo.Customer field need to thread end-to-end into this
// renderer.
func TestFormatPromptPrefix_LoggedIn(t *testing.T) {
	got := FormatPromptPrefix(PromptInfo{User: "alice", Customer: "acme", Cwd: "/"})
	want := "alice@acme ~ $ "
	if got != want {
		t.Errorf("logged-in prompt:\n  got:  %q\n  want: %q", got, want)
	}
}

// TestFormatPromptPrefix_FallbackToWeft covers the pre-login (or dev-
// fixture) case where a user is set but no customer is wired up. The
// renderer must fall back to "@weft" rather than emitting `alice@ `
// (which is what naive customer-only support would do).
func TestFormatPromptPrefix_FallbackToWeft(t *testing.T) {
	got := FormatPromptPrefix(PromptInfo{User: "paul", Cwd: "/"})
	want := "paul@weft ~ $ "
	if got != want {
		t.Errorf("fallback prompt:\n  got:  %q\n  want: %q", got, want)
	}
}

// TestFormatPromptPrefix_NoUser: when there's no user (e.g. shell
// boots before SetUser is called), the prompt skips the user@host
// segment entirely. Without this test the wrong fix would emit
// `@weft ~ $ ` and look broken.
func TestFormatPromptPrefix_NoUser(t *testing.T) {
	got := FormatPromptPrefix(PromptInfo{Cwd: "/"})
	want := "~ $ "
	if got != want {
		t.Errorf("anonymous prompt:\n  got:  %q\n  want: %q", got, want)
	}
}

// TestFormatPromptPrefix_RangeAndCwd makes sure the user@customer
// slot composes with the existing range bracket and cwd segments.
func TestFormatPromptPrefix_RangeAndCwd(t *testing.T) {
	got := FormatPromptPrefix(PromptInfo{
		RangeLabel: "24h",
		User:       "alice",
		Customer:   "acme",
		Cwd:        "/log",
	})
	want := "[24h] alice@acme /log $ "
	if got != want {
		t.Errorf("composed prompt:\n  got:  %q\n  want: %q", got, want)
	}
}
