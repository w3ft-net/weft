package logreads

import (
	"slices"
	"strings"
	"testing"
)

// testEnv is an org with three namespaces and a few /bin scripts.
func testEnv() Env {
	bins := map[string]struct {
		src string
		lua bool
	}{
		"scan":     {src: "cat /log/alpha/api/main.log | grep ERROR"},
		"scanfile": {src: `cat "$1" | grep ERROR`},
		"outer":    {src: "scan; cat /log/beta/b.log"},
		"loopa":    {src: "loopb"},
		"loopb":    {src: "loopa; cat /log/gamma/g.log"},
		"dyn":      {src: `cat $FILE`},
		"fmt.lua":  {src: "print('x')", lua: true},
	}
	return Env{
		Namespaces: []string{"alpha", "beta", "gamma"},
		Bin: func(name string) (string, bool, bool) {
			b, ok := bins[name]
			return b.src, b.lua, ok
		},
	}
}

func TestAnalyze_Resolves(t *testing.T) {
	cases := []struct {
		name, script string
		want         []string // namespaces read
		bins         []string // /bin scripts run
	}{
		{"cat a literal path", "cat /log/alpha/api/main.log | grep ERROR", []string{"alpha"}, nil},
		{"tail with a count", "tail -n 100 /log/alpha/api/main.log", []string{"alpha"}, nil},
		{"a dynamic option value is not a path", `head -n "$N" /log/alpha/api/main.log`, []string{"alpha"}, nil},
		{"a quoted pattern from a variable is not a path", `grep "$LEVEL" /log/alpha/api/main.log`, []string{"alpha"}, nil},
		{"awk variable and program", `awk -v t="$T" '$1 > t' /log/alpha/api/main.log`, []string{"alpha"}, nil},
		{"two namespaces", "cat /log/alpha/a.log /log/beta/b.log", []string{"alpha", "beta"}, nil},
		{"a glob over namespaces reads every match", "cat /log/*/api/main.log", []string{"alpha", "beta", "gamma"}, nil},
		{"a glob inside a name", "cat /log/al*/api/main.log", []string{"alpha"}, nil},
		{"listing /log reads every namespace", "ls /log", []string{"alpha", "beta", "gamma"}, nil},
		{"find under /log", "find /log -name main.log", []string{"alpha", "beta", "gamma"}, nil},
		{"find from the root reaches /log", "find / -name main.log", []string{"alpha", "beta", "gamma"}, nil},
		{"find with no path searches the cwd", "find -name main.log", []string{"alpha", "beta", "gamma"}, nil},
		{"trace with no scope is every namespace", "trace", []string{"alpha", "beta", "gamma"}, nil},
		{"trace in one namespace", "trace alpha --id abc", []string{"alpha"}, nil},
		{"trace in one service", "trace beta/api", []string{"beta"}, nil},
		{"cd into /log then a relative path", "cd /log && cat alpha/api/main.log", []string{"alpha"}, nil},
		{"ls with no operand lists the cwd", "cd /log && ls", []string{"alpha", "beta", "gamma"}, nil},
		{"cd elsewhere then a relative path", "cd /tmp; cat x.log", nil, nil},
		{"other mounts read no logs", "cat /tmp/x /etc/notify.yaml /bin/scan", nil, nil},
		{"echo of a variable", `echo "$HOME" | grep x`, nil, nil},
		{"cron history helpers", "weft::latest errors", nil, nil},
		{"header comments are ignored", "# value: x\n# tag: y\ncat /log/alpha/a.log", []string{"alpha"}, nil},
		{"a read in an if condition", "if grep -q x /log/beta/y; then echo hi; fi", []string{"beta"}, nil},
		{"a read in a subshell", "(cat /log/gamma/z)", []string{"gamma"}, nil},
		{"a read in a command substitution", `X=$(cat /log/beta/y); echo "$X"`, []string{"beta"}, nil},
		{"a read in a function", "f() { cat /log/beta/y; }; f", []string{"beta"}, nil},
		{"a /bin script", "scan", []string{"alpha"}, []string{"scan"}},
		{"a /bin script by path", "/bin/scan", []string{"alpha"}, []string{"scan"}},
		{"a /bin script that runs another", "outer", []string{"alpha", "beta"}, []string{"outer", "scan"}},
		{"scripts that call each other", "loopa", []string{"gamma"}, []string{"loopa", "loopb"}},
		{"a script given a literal argument", "scanfile /log/beta/b.log", []string{"beta"}, []string{"scanfile"}},
		{"a script given no argument", "scanfile", nil, []string{"scanfile"}},
		{"a Lua script reads no logs", "cat /log/alpha/a.log | fmt.lua", []string{"alpha"}, []string{"fmt.lua"}},
		{"xargs of a command that takes no paths", "cat /log/alpha/a.log | xargs echo", []string{"alpha"}, nil},
		{"a relative path from the root", "cat log/alpha/a.log", []string{"alpha"}, nil},
		{"a dot-relative path", "cat ./log/beta/b.log", []string{"beta"}, nil},
		{"dots and doubled slashes are normalized", "cat /log//alpha/../beta/x.log", []string{"beta"}, nil},
		{"cp reads its source", "cp /log/alpha/a.log /tmp/x", []string{"alpha"}, nil},
		{"after --", "cat -- /log/alpha/a.log", []string{"alpha"}, nil},
		{"input redirected from a log", "cat < /log/alpha/a.log", []string{"alpha"}, nil},
		{"a here-string is data", "grep x <<< '/log/alpha/a.log'", nil, nil},
		{"a trailing slash", "ls /log/", []string{"alpha", "beta", "gamma"}, nil},
		{"an unknown command reads nothing", "frobnicate /log/alpha/a.log", []string{"alpha"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Analyze(tc.script, testEnv())
			if err != nil {
				t.Fatalf("Analyze(%q): %v", tc.script, err)
			}
			if !slices.Equal(got.Namespaces, tc.want) {
				t.Errorf("namespaces = %v, want %v", got.Namespaces, tc.want)
			}
			if !slices.Equal(got.Bins, tc.bins) {
				t.Errorf("bins = %v, want %v", got.Bins, tc.bins)
			}
		})
	}
}

// Anything that could name a log path but can't be known when the script is
// saved is refused, with the reason.
func TestAnalyze_RefusesWhatItCannotResolve(t *testing.T) {
	cases := []struct{ name, script, reason string }{
		{"a variable as the path", `cat $FILE`, "FILE"},
		{"a quoted variable as the path", `cat "$FILE"`, "FILE"},
		{"a variable in the namespace", `cat /log/$NS/api/main.log`, "NS"},
		{"a quoted variable in the namespace", `cat "/log/$NS/api/main.log"`, "NS"},
		{"a variable that may spell /log", `cat /lo$X/alpha/a.log`, "X"},
		{"a command substitution as the path", `cat $(echo /log/alpha/a.log)`, "command substitution"},
		{"a computed command", `$CMD /log/alpha/a.log`, "command"},
		{"eval", `eval "cat /log/alpha/a.log"`, "eval"},
		{"source", `source /tmp/x`, "source"},
		{"dot", `. /tmp/x`, "."},
		{"bash -c", `bash -c "cat /log/alpha/a.log"`, "bash"},
		{"alias", `alias c='cat /log/alpha/a.log'`, "alias"},
		{"a loop variable as the path", `for f in /log/alpha/*; do cat $f; done`, "f"},
		{"xargs feeding paths to cat", `find /log/alpha -name x | xargs cat`, "xargs"},
		{"xargs feeding paths to a script", `echo x | xargs scan`, "xargs"},
		{"an unquoted variable can split into more operands", `grep $PAT /log/alpha/a.log`, "PAT"},
		{"a relative path after a computed cd", `cd $DIR; cat main.log`, "directory"},
		{"cd inside a branch", `if true; then cd /log; fi; cat alpha/a.log`, "directory"},
		{"a script that cats its argument, called with a variable", `scanfile "$X"`, "X"},
		{"a script with a computed path", `dyn`, "FILE"},
		{"trace with a computed scope", `trace "$NS"`, "NS"},
		{"a brace expansion in the namespace", `cat /log/{alpha,beta}/a.log`, "brace"},
		{"a variable assigned then used", `FOO=/log/alpha/a.log; cat $FOO`, "FOO"},
		{"a braced variable", `cat ${DIR}/a.log`, "DIR"},
		{"input redirected from a computed path", `grep x < "$FILE"`, "FILE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Analyze(tc.script, testEnv())
			var ue *UnresolvableError
			if !errorsAs(err, &ue) {
				t.Fatalf("Analyze(%q) = %v, want an *UnresolvableError", tc.script, err)
			}
			if !strings.Contains(ue.Reason, tc.reason) {
				t.Errorf("reason = %q, want it to mention %q", ue.Reason, tc.reason)
			}
		})
	}
}

// Commands whose operands are not paths never trip the check, however dynamic.
func TestAnalyze_DynamicWordsInNonPathPositionsAreFine(t *testing.T) {
	for _, script := range []string{
		`echo "$X" "$Y"`,
		`printf '%s\n' "$X"`,
		`test -n "$X" && echo yes`,
		`x="$A$B"; echo ok`,
		`date +%s`,
		`weft::avg "$NAME" 5m`,
	} {
		if _, err := Analyze(script, testEnv()); err != nil {
			t.Errorf("Analyze(%q): %v", script, err)
		}
	}
}

func TestAnalyze_RefusesAScriptThatDoesNotParse(t *testing.T) {
	if _, err := Analyze("cat |", testEnv()); err == nil {
		t.Error("an unparseable script was accepted")
	}
}

// Scripts calling each other in a cycle terminate.
func TestAnalyze_CyclesTerminate(t *testing.T) {
	if _, err := Analyze("loopa", testEnv()); err != nil {
		t.Fatal(err)
	}
}

// A cron names its scripts by path. With RequireBinPath a bare script name is
// refused, in the cron and in the scripts it runs; /bin/<name> resolves.
func TestAnalyze_RequireBinPath(t *testing.T) {
	env := testEnv()
	env.RequireBinPath = true

	reads, err := Analyze("/bin/scan", env)
	if err != nil || !slices.Equal(reads.Bins, []string{"scan"}) {
		t.Fatalf("/bin/scan: %+v, %v", reads, err)
	}
	for _, script := range []string{"scan", "echo hi | scan", "/bin/outer"} { // outer calls scan by its bare name
		_, err := Analyze(script, env)
		var u *UnresolvableError
		if !errorsAs(err, &u) || !strings.Contains(u.Reason, "/bin/scan") {
			t.Errorf("%q: err = %v, want a refusal naming /bin/scan", script, err)
		}
	}
	// Without the flag, a bare name still resolves, as it does for a person at the shell.
	if _, err := Analyze("scan", testEnv()); err != nil {
		t.Errorf("a bare name without RequireBinPath: %v", err)
	}
}
