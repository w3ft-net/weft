package shellcore

// Failing tests for positional-argument support in /bin/ scripts (bash + lua).
//
// Motivating use cases
// ────────────────────
// 1. One disk/cpu alert script, parameterized by log name:
//      /bin/count_errors  ← cat /log/$1 | grep ERROR | wc -l
//      cron A: count_errors disk.log
//      cron B: count_errors cpu.log
// 2. One reusable grep wrapper:
//      /bin/find_in       ← cat $1 | grep $2
//      find_in api.log timeout
// 3. One threshold alert script, parameterized by log + threshold:
//      /bin/above         ← (lua) counts stdin lines, compares to arg[2]
//      above disk.log 100
// 4. Batch fan-out via xargs:
//      echo a.log b.log | xargs -n 1 count_errors
//      → two invocations, each with its own $1
// 5. All-args forwarding for grep/jq/awk wrappers:
//      /bin/search        ← cat vimgym.log | grep -E "$@"
//      search ERROR WARN

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kevlarcon/warp/pkg/bash"
)

// The shellcore→server contract is now "pass /bin invocations through
// verbatim"; the dispatch-layer assertions that used to live here have moved
// to exec_test.go (TestBinFunctionDispatchedByName) and the substantive
// positional-binding tests live server-side:
//   • coordinator/bin_expand_test.go (AST-level pre-expansion)
//   • coordinator/bin_exec_test.go   (coordinator runLocal end-to-end)
//   • warp/pkg/bash/bin_exec_test.go (warp PATH resolution binds $N)
//
// This file retains the warp-level end-to-end tests that exercise the full
// interpreter path a dispatched pipeline hits on the server.

// ─── Warp end-to-end tests ──────────────────────────────────────────────────
//
// Run warp's bash interpreter directly against a virtual FS containing /bin/
// scripts. This is the most accurate test of end-user behavior: warp's own
// /bin PATH resolution should call SetPositional so $N works in the script.
// Today the PATH-resolution path in warp (pkg/bash/exec.go) reads the script
// and calls execScript without ever setting interp.positional, so $N inside
// the script expands to empty.

// luaAwareFS wraps bash.MemFS and tags chosen paths as Lua so warp's PATH
// resolution routes them through the Lua runtime instead of the bash parser.
type luaAwareFS struct {
	*bash.MemFS
	langs map[string]string
}

func (f *luaAwareFS) FileLanguage(path string) string {
	if lang, ok := f.langs[path]; ok {
		return lang
	}
	return "bash"
}

func runBash(t *testing.T, fs bash.FS, stdin, script string) (stdout, stderr string, exit int) {
	t.Helper()
	interp := bash.NewInterpreter()
	interp.SetFS(fs)
	interp.SetStdin(strings.NewReader(stdin))
	var outBuf, errBuf bytes.Buffer
	interp.SetStdout(&outBuf)
	interp.SetStderr(&errBuf)
	bash.RegisterToolCommands(interp)
	ast, err := bash.Parse(script)
	if err != nil {
		t.Fatalf("parse %q: %v", script, err)
	}
	code := interp.ExecScript(ast)
	return outBuf.String(), errBuf.String(), code
}

// TestBinBashExec_EchoDollar1 — the simplest possible test. If this fails,
// bash /bin scripts cannot see their first arg at all.
func TestBinBashExec_EchoDollar1(t *testing.T) {
	fs := bash.NewMemFS()
	fs.WriteFile("/bin/who", []byte("echo $1"))

	out, errOut, code := runBash(t, fs, "", "who alice")
	if code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, errOut)
	}
	if got := strings.TrimSpace(out); got != "alice" {
		t.Errorf("who alice = %q, want alice", got)
	}
}

// TestBinBashExec_ParameterizedLogCount — the user's motivating example,
// end-to-end: one script counts ERROR lines in whichever log is passed.
func TestBinBashExec_ParameterizedLogCount(t *testing.T) {
	fs := bash.NewMemFS()
	fs.WriteFile("/log/disk.log", []byte("INFO boot\nERROR disk full\nERROR io\n"))
	fs.WriteFile("/log/cpu.log", []byte("INFO ok\nINFO ok\n"))
	fs.WriteFile("/bin/count_errors", []byte("cat /log/$1 | grep ERROR | wc -l"))

	diskOut, errOut, code := runBash(t, fs, "", "count_errors disk.log")
	if code != 0 {
		t.Fatalf("disk: exit %d, stderr=%q", code, errOut)
	}
	if got := strings.TrimSpace(diskOut); got != "2" {
		t.Errorf("count_errors disk.log = %q, want 2", got)
	}

	cpuOut, _, _ := runBash(t, fs, "", "count_errors cpu.log")
	if got := strings.TrimSpace(cpuOut); got != "0" {
		t.Errorf("count_errors cpu.log = %q, want 0", got)
	}
}

// TestBinBashExec_TwoArgs — $1 + $2 both bind. Thresholded alert wrapper.
func TestBinBashExec_TwoArgs(t *testing.T) {
	fs := bash.NewMemFS()
	fs.WriteFile("/bin/pair", []byte(`echo "$1 and $2"`))

	out, _, code := runBash(t, fs, "", "pair apple banana")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := strings.TrimSpace(out); got != "apple and banana" {
		t.Errorf("pair apple banana = %q, want `apple and banana`", got)
	}
}

// TestBinBashExec_DollarAt — $@ inside a /bin script should expand to the
// caller's full arg list. Forwarding wrappers (search_errors, find_in_all)
// depend on this.
func TestBinBashExec_DollarAt(t *testing.T) {
	fs := bash.NewMemFS()
	fs.WriteFile("/bin/say", []byte(`echo "$@"`))

	out, _, code := runBash(t, fs, "", "say one two three")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := strings.TrimSpace(out); got != "one two three" {
		t.Errorf("$@ expansion = %q, want `one two three`", got)
	}
}

// TestBinBashExec_XargsFanout — xargs should invoke the script once per
// whitespace-delimited input token, binding each to $1 in turn. This is how
// batch fan-out is expressed in a real shell:
//
//	echo a.log b.log | xargs -n 1 count_errors
//
// Currently warp's xargs is a line-joiner (no command invocation), so this
// test documents the expected behavior that a proper implementation must
// meet. When fixed, it will also exercise positional-arg binding per call.
func TestBinBashExec_XargsFanout(t *testing.T) {
	fs := bash.NewMemFS()
	fs.WriteFile("/log/a.log", []byte("ERROR one\nERROR two\n"))
	fs.WriteFile("/log/b.log", []byte("ERROR only\n"))
	fs.WriteFile("/bin/count_errors", []byte("cat /log/$1 | grep ERROR | wc -l"))

	out, errOut, code := runBash(t, fs, "", `echo a.log b.log | xargs -n 1 count_errors`)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, errOut)
	}
	// Two invocations: first a.log (2 errors), then b.log (1).
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 || fields[0] != "2" || fields[1] != "1" {
		t.Errorf("xargs fan-out output = %q, want two lines: `2` then `1`", out)
	}
}

// ─── Lua ────────────────────────────────────────────────────────────────────

// TestBinLuaExec_ArgTable — Lua's conventional `arg` global should hold
// positional parameters. arg[1] is the first script arg.
func TestBinLuaExec_ArgTable(t *testing.T) {
	fs := &luaAwareFS{
		MemFS: bash.NewMemFS(),
		langs: map[string]string{"/bin/greet": "lua"},
	}
	fs.WriteFile("/bin/greet", []byte(`print(arg[1])`))

	out, errOut, code := runBash(t, fs, "", "greet world")
	if code != 0 {
		t.Fatalf("exit %d, stderr=%q", code, errOut)
	}
	if got := strings.TrimSpace(out); got != "world" {
		t.Errorf("lua arg[1] = %q, want `world`", got)
	}
}

// TestBinLuaExec_MultipleArgs — arg[1] and arg[2] both populated.
func TestBinLuaExec_MultipleArgs(t *testing.T) {
	fs := &luaAwareFS{
		MemFS: bash.NewMemFS(),
		langs: map[string]string{"/bin/pair": "lua"},
	}
	fs.WriteFile("/bin/pair", []byte(`print(arg[1] .. "+" .. arg[2])`))

	out, _, code := runBash(t, fs, "", "pair apple banana")
	if code != 0 {
		t.Fatal(code)
	}
	if got := strings.TrimSpace(out); got != "apple+banana" {
		t.Errorf("pair apple banana = %q, want `apple+banana`", got)
	}
}

// TestBinLuaExec_ParameterizedThreshold — the Lua version of the user's use
// case: one alert script, parameterized by source + threshold. stdin feeds
// rows; arg[1]=name, arg[2]=threshold. Runs twice with different params.
func TestBinLuaExec_ParameterizedThreshold(t *testing.T) {
	fs := &luaAwareFS{
		MemFS: bash.NewMemFS(),
		langs: map[string]string{"/bin/above": "lua"},
	}
	fs.WriteFile("/bin/above", []byte(`
local threshold = tonumber(arg[2])
local n = 0
for _ in lines() do n = n + 1 end
if n > threshold then
    print("ALERT " .. arg[1] .. " " .. n)
else
    print("ok " .. arg[1] .. " " .. n)
end
`))

	// 3 lines, threshold 2 → ALERT
	out, _, _ := runBash(t, fs, "one\ntwo\nthree\n", "above disk 2")
	if got := strings.TrimSpace(out); got != "ALERT disk 3" {
		t.Errorf("above disk 2 (3 lines) = %q, want `ALERT disk 3`", got)
	}

	// 1 line, threshold 2 → ok
	out, _, _ = runBash(t, fs, "only\n", "above cpu 2")
	if got := strings.TrimSpace(out); got != "ok cpu 1" {
		t.Errorf("above cpu 2 (1 line) = %q, want `ok cpu 1`", got)
	}
}

// TestBinLuaExec_ArgCount — #arg reports how many positional args were
// supplied. Convention: Lua's length operator on the arg table.
func TestBinLuaExec_ArgCount(t *testing.T) {
	fs := &luaAwareFS{
		MemFS: bash.NewMemFS(),
		langs: map[string]string{"/bin/argc": "lua"},
	}
	fs.WriteFile("/bin/argc", []byte(`print(#arg)`))

	out, _, _ := runBash(t, fs, "", "argc a b c d")
	if got := strings.TrimSpace(out); got != "4" {
		t.Errorf("#arg for `argc a b c d` = %q, want 4", got)
	}
}
