package coordinator

import (
	"testing"

	"github.com/kevlarcon/warp/pkg/bash"
)

// testFS satisfies bash.FS + bash.LangFS by delegating most methods to an
// embedded MemFS and tagging per-path languages.
type testFS struct {
	*bash.MemFS
	langs map[string]string
}

func newTestFS() *testFS {
	return &testFS{MemFS: bash.NewMemFS(), langs: map[string]string{}}
}

func (f *testFS) setBash(path, body string) {
	f.WriteFile(path, []byte(body))
}

func (f *testFS) setLua(path, body string) {
	f.WriteFile(path, []byte(body))
	f.langs[path] = "lua"
}

func (f *testFS) FileLanguage(path string) string {
	if l, ok := f.langs[path]; ok {
		return l
	}
	return "bash"
}

func TestExpandBinPipeline_SubstitutesPositionalSingleArg(t *testing.T) {
	fs := newTestFS()
	fs.setBash("/bin/count_errors", `cat /log/$1 | grep ERROR | wc -l`)

	got, err := expandBinPipeline("count_errors disk.log", fs)
	if err != nil {
		t.Fatal(err)
	}
	want := "cat /log/disk.log | grep ERROR | wc -l"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExpandBinPipeline_SplicesIntoOuterPipeline(t *testing.T) {
	fs := newTestFS()
	fs.setBash("/bin/errors", `cat /log/$1 | grep ERROR`)

	got, err := expandBinPipeline("errors disk.log | wc -l", fs)
	if err != nil {
		t.Fatal(err)
	}
	want := "cat /log/disk.log | grep ERROR | wc -l"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExpandBinPipeline_LuaScriptLeftAlone(t *testing.T) {
	fs := newTestFS()
	fs.setLua("/bin/greet", `print(arg[1])`)

	got, err := expandBinPipeline("greet world", fs)
	if err != nil {
		t.Fatal(err)
	}
	if got != "greet world" {
		t.Errorf("lua should pass through; got %q", got)
	}
}

func TestExpandBinPipeline_NotABinScript(t *testing.T) {
	fs := newTestFS()
	got, err := expandBinPipeline("echo hello | wc -l", fs)
	if err != nil {
		t.Fatal(err)
	}
	if got != "echo hello | wc -l" {
		t.Errorf("unknown command should pass through; got %q", got)
	}
}

func TestExpandBinPipeline_MultipleArgs(t *testing.T) {
	fs := newTestFS()
	fs.setBash("/bin/find_in", `cat /log/$1 | grep $2`)

	got, err := expandBinPipeline("find_in api.log timeout", fs)
	if err != nil {
		t.Fatal(err)
	}
	want := "cat /log/api.log | grep timeout"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The integration test the user cares about: one script, two different
// log names. Each expanded pipeline references its own log.
func TestExpandBinPipeline_SameScriptDifferentArgs(t *testing.T) {
	fs := newTestFS()
	fs.setBash("/bin/count_errors", `cat /log/$1 | grep ERROR | wc -l`)

	disk, _ := expandBinPipeline("count_errors disk.log", fs)
	cpu, _ := expandBinPipeline("count_errors cpu.log", fs)

	if disk == cpu {
		t.Fatalf("same output for different args: %q", disk)
	}
	if disk != "cat /log/disk.log | grep ERROR | wc -l" {
		t.Errorf("disk = %q", disk)
	}
	if cpu != "cat /log/cpu.log | grep ERROR | wc -l" {
		t.Errorf("cpu = %q", cpu)
	}
}

func TestExpandBinPipeline_NilFS(t *testing.T) {
	got, err := expandBinPipeline("foo bar", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "foo bar" {
		t.Errorf("nil fs should pass through; got %q", got)
	}
}

func TestExpandBinPipeline_MultiListBodyPassThrough(t *testing.T) {
	// Bodies with multiple lists can't be cleanly spliced into a
	// pipeline — fall back to runtime resolution.
	fs := newTestFS()
	fs.setBash("/bin/seq", `echo $1 && echo $2`)

	got, _ := expandBinPipeline("seq a b", fs)
	if got != "seq a b" {
		t.Errorf("multi-list body should pass through; got %q", got)
	}
}
