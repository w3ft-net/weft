package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/w3ft-net/weft/cmd/weft/ui"
	"github.com/w3ft-net/weft/shell"
)

func run(t *testing.T, opts Options, args ...string) (string, error) {
	t.Helper()
	root, _ := NewRoot(opts)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// A bare weft lists the subcommands; it does not start the shell.
func TestBareWeftListsTheSubcommands(t *testing.T) {
	out, err := run(t, Options{ClientID: "weft/test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sh", "namespace", "source", "token", "mcp", "propose", "version"} {
		if !strings.Contains(out, "  "+want+" ") {
			t.Errorf("subcommand %q is not listed:\n%s", want, out)
		}
	}
}

func TestVersionPrintsTheClientID(t *testing.T) {
	out, err := run(t, Options{ClientID: "weft/1.2.3"}, "version")
	if err != nil || strings.TrimSpace(out) != "weft/1.2.3" {
		t.Fatalf("version = %q, %v", out, err)
	}
}

// --nocolor is global: it parses wherever it appears, and --no-color is the same flag.
func TestNoColorIsAGlobalFlag(t *testing.T) {
	defer func(n bool) { ui.NoColor = n }(ui.NoColor)
	for _, args := range [][]string{
		{"--nocolor", "version"},
		{"version", "--nocolor"},
		{"--no-color", "version"},
	} {
		ui.NoColor = false
		if _, err := run(t, Options{ClientID: "x"}, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !ui.NoColor {
			t.Errorf("%v did not set NoColor", args)
		}
	}
}

// A binary's PreRun hook sees every command.
func TestPreRunSeesTheCommand(t *testing.T) {
	var seen []string
	_, err := run(t, Options{PreRun: func(cmd *cobra.Command, _ []string) { seen = append(seen, cmd.Name()) }}, "version")
	if err != nil || len(seen) != 1 || seen[0] != "version" {
		t.Fatalf("seen = %v, %v", seen, err)
	}
}

func TestParseTarget(t *testing.T) {
	ns, svc, err := parseTarget("weft/shard/main")
	if err != nil || ns != "weft" || svc != "shard/main" {
		t.Errorf("parseTarget = %q %q %v", ns, svc, err)
	}
	for _, bad := range []string{"", "ns", "ns/", "/svc"} {
		if _, _, err := parseTarget(bad); err == nil {
			t.Errorf("parseTarget(%q) accepted", bad)
		}
	}
}

// The old flag spellings are not accepted: a stray flag must not silently
// widen what a command acts on.
func TestUnknownFlagIsAnError(t *testing.T) {
	for _, args := range [][]string{
		{"source", "pending", "--namespace", "ops"},
		{"source", "remove", "--ns", "ops", "svc"},
		{"token", "list", "--bogus"},
	} {
		if _, err := run(t, Options{}, args...); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}

func TestArgumentShapes(t *testing.T) {
	for _, args := range [][]string{
		{"source", "add", "ops/api"},     // --locator is required
		{"source", "remove", "ops"},      // not ns/svc
		{"namespace", "create", "a/b"},   // a namespace has no slash
		{"token", "create"},              // needs a namespace
		{"token", "revoke"},              // --id is required
		{"sh", "--range", "1h.."},        // --range needs -c
		{"source", "migrate", "ops/api"}, // --lines is required
		{"source", "pending", "a", "b"},  // at most one namespace
	} {
		if _, err := run(t, Options{}, args...); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}

func TestBasicCredential(t *testing.T) {
	t.Setenv("WEFT_BASIC_AUTH_USER", "")
	t.Setenv("WEFT_BASIC_AUTH_PASSWORD", "")
	if _, err := (Basic{}).Credential(&shell.Config{}); err == nil {
		t.Error("no credential configured, but Basic produced one")
	}
	t.Setenv("WEFT_BASIC_AUTH_USER", "u")
	t.Setenv("WEFT_BASIC_AUTH_PASSWORD", "p")
	got, err := (Basic{}).Credential(&shell.Config{})
	if err != nil || got != "Basic dTpw" {
		t.Errorf("credential = %q, %v", got, err)
	}
	if h := shell.Authorization(got); h != "Basic dTpw" {
		t.Errorf("Authorization = %q", h)
	}
}
