package store

import (
	"strings"
	"testing"
)

func TestValidateSource_BashOK(t *testing.T) {
	if err := validateSource("bash", `echo hello | wc -l`); err != nil {
		t.Errorf("valid bash rejected: %v", err)
	}
}

func TestValidateSource_BashSyntaxError(t *testing.T) {
	// Candidates that should trigger warp's bash parser. Not every
	// pathological input is rejected (some lexers recover), so probe a
	// few; at least one must fail. If warp grows stricter later, any
	// that start failing are fine.
	candidates := []string{
		`if then fi`,           // if with no condition
		`echo hello; done`,     // dangling done
		`echo "unterminated`,   // unterminated double quote
		`echo 'unterminated`,   // unterminated single quote
		`echo $(`,              // unterminated command sub
		`for; do echo x; done`, // for without name
	}
	var rejected []string
	for _, c := range candidates {
		if err := validateSource("bash", c); err != nil {
			if !strings.Contains(err.Error(), "bash syntax") {
				t.Errorf("err for %q = %q, want `bash syntax` prefix", c, err)
			}
			rejected = append(rejected, c)
		}
	}
	if len(rejected) == 0 {
		t.Fatalf("warp's bash.Parse accepted every malformed sample; candidates=%v", candidates)
	}
	t.Logf("rejected: %v", rejected)
}

func TestValidateSource_LuaOK(t *testing.T) {
	if err := validateSource("lua", `print(arg[1])`); err != nil {
		t.Errorf("valid lua rejected: %v", err)
	}
}

func TestValidateSource_LuaSyntaxError(t *testing.T) {
	err := validateSource("lua", `print(arg[1]`) // missing close paren
	if err == nil {
		t.Fatal("expected syntax error, got nil")
	}
	if !strings.Contains(err.Error(), "lua syntax") {
		t.Errorf("err=%q, want lua syntax prefix", err)
	}
}

func saveTestSetup(t *testing.T) (*Store, uint) {
	t.Helper()
	db := testOpen(t)
	cust, err := db.FindOrCreateCustomer("save-validate-test")
	if err != nil {
		t.Fatal(err)
	}
	return db, cust.ID
}

func TestSaveShellFunction_RejectsInvalidBash(t *testing.T) {
	db, custID := saveTestSetup(t)
	// Use a form TestValidateSource_BashSyntaxError proves warp rejects.
	err := db.SaveShellFunction(custID, "broken", `echo hello; done`)
	if err == nil {
		t.Fatal("expected error saving broken bash")
	}
	if _, err := db.GetShellFunction(custID, "broken"); err == nil {
		t.Error("broken script was persisted despite syntax error")
	}
}

func TestSaveShellFunction_RejectsInvalidLua(t *testing.T) {
	db, custID := saveTestSetup(t)
	err := db.SaveShellFunction(custID, "bad", "#!/usr/bin/env lua\nprint(arg[1]")
	if err == nil {
		t.Fatal("expected error saving broken lua")
	}
	if _, err := db.GetShellFunction(custID, "bad"); err == nil {
		t.Error("broken lua was persisted despite syntax error")
	}
}
