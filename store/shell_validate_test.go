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
	err := db.SaveShellFunction(custID, "", "broken", `echo hello; done`, "bash")
	if err == nil {
		t.Fatal("expected error saving broken bash")
	}
	if _, err := db.GetShellFunction(custID, "broken"); err == nil {
		t.Error("broken script was persisted despite syntax error")
	}
}

func TestSaveShellFunction_RejectsInvalidLua(t *testing.T) {
	db, custID := saveTestSetup(t)
	// Lua source passed pre-stripped (caller's job now that the store
	// no longer detects). Source ends with an unclosed bracket so the
	// parser rejects it.
	err := db.SaveShellFunction(custID, "", "bad", "print(arg[1]", "lua")
	if err == nil {
		t.Fatal("expected error saving broken lua")
	}
	if _, err := db.GetShellFunction(custID, "bad"); err == nil {
		t.Error("broken lua was persisted despite syntax error")
	}
}

// The terminal-alert-only rule: alert may appear only as the final
// stage of a /bin body's final top-level list, and that list must be
// a bare pipeline. Everything else is rejected at save time so the
// interceptor never has to reason about alert position at runtime.

func TestValidateSource_AcceptsTerminalAlert(t *testing.T) {
	cases := []string{
		// Bare single-pipeline body.
		`cat /log/api.log | grep ERROR | wc -l | alert above 10`,
		// Multi-list body, alert on the trailing pipeline.
		`echo starting
cat /log/api.log | grep ERROR | wc -l | alert above 10`,
		// Body with several lists; alert only on the last.
		`echo a
echo b
cat /log/api.log | wc -l | alert above 0`,
		// No alert at all — the common case must keep working.
		`echo starting
cat /log/api.log | grep ERROR | wc -l
echo finished`,
	}
	for _, src := range cases {
		if err := validateSource("bash", src); err != nil {
			t.Errorf("valid body rejected: %v\n---\n%s", err, src)
		}
	}
}

func TestValidateSource_RejectsInteriorAlert(t *testing.T) {
	cases := map[string]string{
		"alert in first list of multi-list body":   "cat /log/api.log | wc -l | alert above 10\necho finished",
		"alert mid-body":                           "echo a\ncat /log/api.log | wc -l | alert above 0\necho b",
		"alert inside for loop":                    "for a in x y; do cat /log/$a.log | wc -l | alert above 0; done",
		"alert inside if body":                     "if true; then cat /log/api.log | wc -l | alert above 0; fi",
		"alert inside subshell":                    "( cat /log/api.log | wc -l | alert above 0 )",
		"alert inside command group":               "{ cat /log/api.log | wc -l | alert above 0; }",
		"alert after final list's && continuation": "cat /log/api.log | wc -l && echo ok | alert above 0",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateSource("bash", src)
			if err == nil {
				t.Errorf("expected rejection for interior alert; source was:\n%s", src)
				return
			}
			if !strings.Contains(err.Error(), "alert may only appear") {
				t.Errorf("err=%q, want message about alert placement", err)
			}
		})
	}
}

// A body with no alert at all (but with interior source pipelines)
// must also pass validation — this is the hot path for the interceptor
// feature's motivating case.
func TestValidateSource_AllowsBodyWithoutAlertAndInteriorQueries(t *testing.T) {
	src := `echo starting
cat /log/api.log | grep ERROR | wc -l
for app in api web; do
  cat /log/$app.log | grep WARN | wc -l
done
echo finished`
	if err := validateSource("bash", src); err != nil {
		t.Errorf("valid alert-free body rejected: %v", err)
	}
}

// `tail -f` is banned in saved scripts. Follow mode streams
// indefinitely — no natural completion point — so a saved script
// containing it would hang forever from the caller's perspective.
// The ban catches all common spellings.
func TestValidateSource_RejectsTailFollow(t *testing.T) {
	cases := map[string]string{
		"tail -f":                  `tail -f /log/api.log`,
		"tail -f with grep":        `tail -f /log/api.log | grep ERROR`,
		"tail --follow long form":  `tail --follow /log/api.log`,
		"tail -fn 100 (joined)":    `tail -fn 100 /log/api.log`,
		"tail -f in for loop":      `for a in api web; do tail -f /log/$a.log; done`,
		"tail -f inside cmdsub":    "x=$(tail -f /log/api.log); echo $x",
		"tail -f inside function":  "watch() { tail -f /log/$1.log; }\nwatch api",
		"tail -f after echo":       "echo starting\ntail -f /log/api.log",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateSource("bash", src)
			if err == nil {
				t.Errorf("expected rejection; source was:\n%s", src)
				return
			}
			if !strings.Contains(err.Error(), "tail -f is not allowed") {
				t.Errorf("err=%q, want message about tail -f", err)
			}
		})
	}
}

// Plain `tail` (without -f) is still allowed — it's a normal bounded
// read of the last N lines.
func TestValidateSource_AllowsPlainTail(t *testing.T) {
	cases := []string{
		`tail /log/api.log`,
		`tail -n 100 /log/api.log`,
		`tail -100 /log/api.log`,
		`tail -n 50 /log/api.log | grep ERROR`,
	}
	for _, src := range cases {
		if err := validateSource("bash", src); err != nil {
			t.Errorf("valid `tail` without -f rejected: %v\nsrc: %s", err, src)
		}
	}
}

// `range` is banned in saved scripts: it modifies the request's
// time window, which a script cannot sensibly scope.
func TestValidateSource_RejectsRangeBuiltin(t *testing.T) {
	cases := map[string]string{
		"bare range":          `range 1h..`,
		"range in for loop":   `for t in 1h 2h; do range $t..; cat /log/api.log | wc -l; done`,
		"range inside cmdsub": "x=$(range 30m..; cat /log/api.log | wc -l); echo $x",
		"range after echo":    "echo starting\nrange 1h..\ncat /log/api.log | wc -l",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateSource("bash", src)
			if err == nil {
				t.Errorf("expected rejection; source was:\n%s", src)
				return
			}
			if !strings.Contains(err.Error(), "range is not allowed") {
				t.Errorf("err=%q, want message about range", err)
			}
		})
	}
}

// `range` as an argument to another command is fine — it's a common
// enough word that we shouldn't false-positive on `echo range` or
// a variable named `$range`.
func TestValidateSource_AllowsRangeAsArgument(t *testing.T) {
	cases := []string{
		`echo range`,
		`echo "the range is"`,
		`grep range /log/api.log`,
		`x=range; echo $x`,
	}
	for _, src := range cases {
		if err := validateSource("bash", src); err != nil {
			t.Errorf("valid use of `range` as arg rejected: %v\nsrc: %s", err, src)
		}
	}
}
