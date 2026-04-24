package coordinator

import (
	"strings"
	"testing"
)

// TestRunLocal_BinBashScriptResolution verifies that /bin/ bash scripts
// resolve server-side through the coordinator's ShellFS-backed interpreter.
// Before the wiring, bash.NewInterpreter had no FS so warp emitted
// "command not found" for every /bin invocation.
func TestRunLocal_BinBashScriptResolution(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	if err := db.SaveShellFunction(custID, "who", "echo $1"); err != nil {
		t.Fatalf("save: %v", err)
	}

	c := &Coordinator{Store: db}
	result, err := c.runLocal("who alice", TimeRange{}, "", custID, nil)
	if err != nil {
		t.Fatalf("runLocal: %v", err)
	}
	if got := strings.TrimSpace(result.Text); got != "alice" {
		t.Errorf("runLocal(who alice) stdout = %q, want alice (stderr=%q)", got, result.Stderr)
	}
}

// TestRunLocal_BinLuaScriptResolution verifies the coordinator resolves
// Lua /bin scripts too. This needs two things working together: the
// ShellFS LangFS hook telling warp the script is lua, and the Lua sandbox
// accepting a shebang-prefixed source (ShellFS re-prepends the shebang
// on read for display fidelity).
func TestRunLocal_BinLuaScriptResolution(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	src := "#!/usr/bin/env lua\nprint(arg[1])"
	if err := db.SaveShellFunction(custID, "greet", src); err != nil {
		t.Fatalf("save: %v", err)
	}

	c := &Coordinator{Store: db}
	result, err := c.runLocal("greet world", TimeRange{}, "", custID, nil)
	if err != nil {
		t.Fatalf("runLocal: %v", err)
	}
	if got := strings.TrimSpace(result.Text); got != "world" {
		t.Errorf("runLocal(greet world) stdout = %q, want world (stderr=%q)", got, result.Stderr)
	}
}

// TestRunLocal_BinBashPositionalRebinds — the user's motivating use case
// at the coordinator level: one script, invoked with different args,
// binds $1 per call.
func TestRunLocal_BinBashPositionalRebinds(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	if err := db.SaveShellFunction(custID, "greet", `echo "hello $1"`); err != nil {
		t.Fatalf("save: %v", err)
	}
	c := &Coordinator{Store: db}

	alice, err := c.runLocal("greet alice", TimeRange{}, "", custID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(alice.Text); got != "hello alice" {
		t.Errorf("greet alice = %q, want `hello alice`", got)
	}

	bob, err := c.runLocal("greet bob", TimeRange{}, "", custID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(bob.Text); got != "hello bob" {
		t.Errorf("greet bob = %q, want `hello bob`", got)
	}
}
