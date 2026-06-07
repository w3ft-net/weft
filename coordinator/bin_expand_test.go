package coordinator

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/warp/pkg/bash"

	"github.com/graybeardcorp/weft/planner"
	"time"
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

// TestExpandBinPipeline_QuotedPositionalWithSpaces is the regression
// for the alert-triage bug: a cron pipeline like
//
//	log-pattern-count coordinator 500 'compute exit 127' | alert above 0
//
// expanded to a body that ran `grep -c "$3"` was firing
// "compute exit 127: grep: missing pattern" on the compute pool —
// $3 was being lost across the splice boundary so the inner grep
// got an empty pattern. The fix is for expandBinPipeline to preserve
// the quoted-arg semantics through positional binding so the unparsed
// pipeline still has the multi-word literal as a single grep argument.
func TestExpandBinPipeline_QuotedPositionalWithSpaces(t *testing.T) {
	fs := newTestFS()
	fs.setBash("/bin/log-pattern-count", `tail -n $2 /log/weft/$1/main.log | grep -c "$3"`)

	got, err := expandBinPipeline(`log-pattern-count coordinator 500 'compute exit 127'`, fs)
	if err != nil {
		t.Fatal(err)
	}
	want := `tail -n 500 /log/weft/coordinator/main.log | grep -c "compute exit 127"`
	if got != want {
		t.Errorf("\n got: %s\nwant: %s", got, want)
	}

	// Sanity: re-parse the expansion to confirm grep sees one
	// positional, not three. A regression where we lose the quotes
	// would surface here as len(grep.Words) == 4 ('grep', '-c',
	// 'compute', 'exit', '127') instead of 3.
	ast, err := bash.Parse(got)
	if err != nil {
		t.Fatalf("re-parse expanded pipeline: %v", err)
	}
	cmds := ast.Commands[0].First.Commands
	grep, ok := cmds[len(cmds)-1].(*bash.SimpleCommand)
	if !ok {
		t.Fatalf("last stage is not a SimpleCommand: %T", cmds[len(cmds)-1])
	}
	// grep, -c, "compute exit 127" → 3 words.
	if len(grep.Words) != 3 {
		t.Errorf("grep got %d words, want 3 (quoted-arg should stay one): %v",
			len(grep.Words),
			func() []string {
				ws := make([]string, len(grep.Words))
				for i, w := range grep.Words {
					ws[i] = w.Literal()
				}
				return ws
			}())
	}
	if grep.Words[len(grep.Words)-1].Literal() != "compute exit 127" {
		t.Errorf("grep last arg = %q, want %q",
			grep.Words[len(grep.Words)-1].Literal(), "compute exit 127")
	}
}

// TestExpandBinPipeline_QuotedArgRoundTripsToPlanner takes the
// expanded pipeline, hands it to planner.Plan, and confirms that
// the compute-stage formatter emits `grep -c 'compute exit 127'`
// (single quoted arg) rather than `grep -c compute exit 127`
// (word-split). joinArgs (planner/planner.go) is supposed to
// re-quote whitespace-bearing args; this pins that.
func TestExpandBinPipeline_QuotedArgRoundTripsToPlanner(t *testing.T) {
	fs := newTestFS()
	fs.setBash("/bin/log-pattern-count", `tail -n $2 /log/weft/$1/main.log | grep -c "$3"`)
	expanded, err := expandBinPipeline(`log-pattern-count coordinator 500 'compute exit 127'`, fs)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := planner.Plan(expanded, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("planner.Plan(%q): %v", expanded, err)
	}

	if len(plan.ComputeStages) != 1 {
		t.Fatalf("expected 1 compute stage, got %d: %v",
			len(plan.ComputeStages), plan.ComputeStages)
	}
	stage := plan.ComputeStages[0]
	want := `grep -c 'compute exit 127'`
	if stage != want {
		t.Errorf("compute stage\n got: %q\nwant: %q", stage, want)
	}
}

// TestComputeStage_PreservesShellMetacharacters is the actual prod
// symptom: a cron pipeline whose `grep '(empty)'` argument has no
// whitespace but is full of shell metachars. joinArgs quotes only
// whitespace-containing args, so the formatted compute stage is
// `grep (empty)` — and when the compute pool re-joins stages with
// " | " and re-parses the script, the bare parens become a
// subshell, leaving grep with zero positionals → "missing pattern".
//
// The fix is to quote any arg that contains a shell metacharacter,
// not just whitespace. Failing version of this test pins the bug:
// after Plan(), we re-parse the joined compute script and check
// that grep has the expected literal positional arg.
func TestComputeStage_PreservesShellMetacharacters(t *testing.T) {
	pipeline := `tail -n 100 /log/weft/shard/metrics.log | jq -r '._node // "(empty)"' | sort | uniq -c | grep '(empty)' | awk '{print $1}'`

	plan, err := planner.Plan(pipeline, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	script := strings.Join(plan.ComputeStages, " | ")
	t.Logf("compute script = %q", script)

	ast, err := bash.Parse(script)
	if err != nil {
		t.Fatalf("re-parse compute script: %v\nscript=%q", err, script)
	}
	if len(ast.Commands) != 1 {
		t.Fatalf("expected 1 list, got %d (subshell parse leak?)", len(ast.Commands))
	}
	cmds := ast.Commands[0].First.Commands

	// Find the grep stage by name.
	var grepCmd *bash.SimpleCommand
	for _, c := range cmds {
		sc, ok := c.(*bash.SimpleCommand)
		if !ok || len(sc.Words) == 0 {
			continue
		}
		if sc.Words[0].Literal() == "grep" {
			grepCmd = sc
			break
		}
	}
	if grepCmd == nil {
		t.Fatalf("no grep stage in re-parsed compute script: %q", script)
	}
	// Should be: grep, (empty)
	if len(grepCmd.Words) != 2 {
		t.Errorf("grep got %d words, want 2; script=%q", len(grepCmd.Words), script)
	}
	if got := grepCmd.Words[len(grepCmd.Words)-1].Literal(); got != "(empty)" {
		t.Errorf("grep last arg = %q, want %q (script=%q)", got, "(empty)", script)
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
