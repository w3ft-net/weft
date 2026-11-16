//go:build test

package coordtest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/logdb"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// BinQueries runs saved /bin scripts through the coordinator: they resolve and
// bind their arguments, and a log query anywhere in one (before and after
// other commands, in a loop, a command substitution, a function, a redirect) is
// answered by the shards, as is a script used as a stage after a log read.
func BinQueries(t *testing.T, newSubject func(t *testing.T) Subject) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, *queryFixture)
	}{
		{"BashScriptResolution", binBashResolution},
		{"LuaScriptResolution", binLuaResolution},
		{"BashPositionalRebinds", binPositionalRebinds},
		{"InteriorEchoBeforeAndAfter", interiorEchoBeforeAndAfter},
		{"InteriorPositionalArg", interiorPositionalArg},
		{"InteriorInsideForLoop", interiorForLoop},
		{"InteriorRedirectInBinBody", interiorRedirect},
		{"InteriorCommandSubstitution", interiorCommandSubstitution},
		{"InteriorAndShortCircuit", interiorAndShortCircuit},
		{"InteriorFunctionWithConditionalReturn", interiorFunctionConditional},
		{"InteriorFunctionDefinedInSeparateBinScript", interiorFunctionAcrossScripts},
		{"InteriorSinglePipelineStillSplices", interiorSinglePipeline},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, newInteriorFixture(t, newSubject(t))) })
	}
}

// BinDownstream is a /bin script as a later stage after a log read. The stages
// of such a pipeline run concurrently and each reaches the stores, so the
// Subject must not be a single connection: a build that keeps its test state in
// one transaction passes a pooled one.
func BinDownstream(t *testing.T, newSubject func(t *testing.T) Subject) {
	t.Run("ScriptAfterLogRead", func(t *testing.T) { binAfterLogRead(t, newInteriorFixture(t, newSubject(t))) })
	t.Run("RedirectStaysOnLastStage", func(t *testing.T) { binAfterLogReadRedirect(t, newInteriorFixture(t, newSubject(t))) })
}

// newInteriorFixture is a coordinator over one shard holding, for the last
// hour, 2 ERROR rows and 1 INFO row of namespace api, and 1 ERROR and 1 WARN of
// web.
func newInteriorFixture(t *testing.T, s Subject) *queryFixture {
	t.Helper()
	f := newBareFixture(t, s)
	for _, ns := range []string{"api", "web"} {
		n, err := s.Sources.FindOrCreateNamespace(s.Owner, ns)
		if err != nil {
			t.Fatal(err)
		}
		f.namespaces[ns] = n.ID
	}
	live, err := shard.NewDuckDBShard("interior-test", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })

	now := time.Now()
	row := func(app, level string, ts time.Time) *pb.LogRow {
		ns, svc, kind := shard.SplitApp(app)
		r := &pb.LogRow{Raw: level + " " + app + " line", TsUnixNs: ts.UnixNano(), Level: level, Namespace: ns, Service: svc, Kind: kind, Instance: "i-1"}
		if s.Stamp != nil {
			s.Stamp(r)
		}
		return r
	}
	if err := live.Ingest(context.Background(), []*pb.LogRow{
		row("api", "ERROR", now.Add(-10*time.Minute)),
		row("api", "ERROR", now.Add(-8*time.Minute)),
		row("api", "INFO", now.Add(-6*time.Minute)),
		row("web", "ERROR", now.Add(-5*time.Minute)),
		row("web", "WARN", now.Add(-2*time.Minute)),
	}, nil); err != nil {
		t.Fatal(err)
	}
	f.registerServing(t, &logdb.Shard{
		ShardRow: logdb.ShardRow{ID: live.ID(), State: logdb.StateIngesting, CreatedAt: now},
		Live:     live,
	}, "api", "web")
	return f
}

// run executes line over the last hour and returns what it produced.
func (f *queryFixture) run(t *testing.T, line string) *coordinator.DrainedResult {
	t.Helper()
	now := time.Now()
	res, err := f.c.Execute(context.Background(), line, coordinator.TimeRange{Start: now.Add(-time.Hour), End: now.Add(time.Minute)}, f.s.PrincipalOf(""))
	if err != nil {
		t.Fatalf("Execute %q: %v", line, err)
	}
	d, err := res.Drain(context.Background())
	if err != nil {
		t.Fatalf("drain %q: %v", line, err)
	}
	return d
}

// script saves a bash /bin script.
func (f *queryFixture) script(t *testing.T, name, body string) {
	t.Helper()
	f.s.saveBin(t, name, body)
}

// outputLines is the lines of d's text, which must be want long.
func outputLines(t *testing.T, d *coordinator.DrainedResult, want int) []string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(d.Text), "\n")
	if len(lines) != want {
		t.Fatalf("want %d output lines, got %d: %q (stderr %q)", want, len(lines), d.Text, d.Stderr)
	}
	return lines
}

func binBashResolution(t *testing.T, f *queryFixture) {
	f.script(t, "who", "echo $1")
	if d := f.run(t, "who alice"); strings.TrimSpace(d.Text) != "alice" {
		t.Errorf("who alice stdout = %q, want alice (stderr %q)", d.Text, d.Stderr)
	}
}

// A Lua script resolves too: the filesystem tells the interpreter the script is
// Lua, and the Lua sandbox accepts the shebang the filesystem re-prepends when
// the script is read.
func binLuaResolution(t *testing.T, f *queryFixture) {
	if err := f.s.Bins.Save(f.s.Owner, "", "greet", "print(arg[1])", "lua"); err != nil {
		t.Fatal(err)
	}
	if d := f.run(t, "greet world"); strings.TrimSpace(d.Text) != "world" {
		t.Errorf("greet world stdout = %q, want world (stderr %q)", d.Text, d.Stderr)
	}
}

// One script invoked with different arguments binds $1 per call.
func binPositionalRebinds(t *testing.T, f *queryFixture) {
	f.script(t, "greet", `echo "hello $1"`)
	for arg, want := range map[string]string{"alice": "hello alice", "bob": "hello bob"} {
		if got := strings.TrimSpace(f.run(t, "greet "+arg).Text); got != want {
			t.Errorf("greet %s = %q, want %q", arg, got, want)
		}
	}
}

// An echo preamble, a query whose predicate pushes down to SQL, an echo
// postamble. Without the interceptor only the echos fired: the query ran through
// native bash over an empty /log and printed 0.
func interiorEchoBeforeAndAfter(t *testing.T, f *queryFixture) {
	f.script(t, "scan", "echo starting\ncat /log/api.log | grep ERROR | wc -l\necho finished")
	lines := outputLines(t, f.run(t, "scan"), 3)
	if lines[0] != "starting" || lines[1] != "2" || lines[2] != "finished" {
		t.Errorf("got %q, want starting, 2, finished (the interceptor is likely not firing)", lines)
	}
}

// Positional arguments bind per invocation: the interceptor sees the
// fully-expanded pipeline, cat /log/api.log and not cat /log/$1.
func interiorPositionalArg(t *testing.T, f *queryFixture) {
	f.script(t, "scan", "echo starting\ncat /log/$1.log | grep ERROR | wc -l")
	for arg, want := range map[string]string{"api": "2", "web": "1"} {
		d := f.run(t, "scan "+arg)
		if lines := strings.Split(strings.TrimSpace(d.Text), "\n"); len(lines) < 2 || lines[1] != want {
			t.Errorf("scan %s: %q, want ERROR count %s (stderr %q)", arg, d.Text, want, d.Stderr)
		}
	}
}

// A for loop is what defeats static pre-expansion: $app cannot be resolved until
// the loop runs, which is why the hook is at runtime. It sees one dispatch per
// iteration, each with its own value.
func interiorForLoop(t *testing.T, f *queryFixture) {
	f.script(t, "loop", "for app in api web; do\n  cat /log/$app.log | grep ERROR | wc -l\ndone")
	lines := outputLines(t, f.run(t, "loop"), 2)
	if lines[0] != "2" || lines[1] != "1" {
		t.Errorf("per-iteration counts %q, want api 2 then web 1", lines)
	}
}

// A redirect in a script body lands the captured rows in /tmp rather than
// leaking them to stdout: substituting the pipeline with a callback once dropped
// the redirect.
func interiorRedirect(t *testing.T, f *queryFixture) {
	f.script(t, "snapshot", "cat /log/api.log > /tmp/snapshot\necho finished")
	if got := strings.TrimSpace(f.run(t, "snapshot").Text); got != "finished" {
		t.Errorf("script stdout = %q, want finished (the redirect leaked output back to stdout)", got)
	}
	contents := f.run(t, "cat /tmp/snapshot").Text
	if !strings.Contains(contents, "ERROR api line") || !strings.Contains(contents, "INFO api line") {
		t.Errorf("/tmp/snapshot is missing the api rows (the redirect was probably dropped):\n%s", contents)
	}
	if strings.Contains(contents, "web line") {
		t.Errorf("/tmp/snapshot leaked web rows:\n%s", contents)
	}
}

// The inner pipeline of a command substitution is query-shaped, runs through
// the interceptor, and its stdout lands in the assignment.
func interiorCommandSubstitution(t *testing.T, f *queryFixture) {
	f.script(t, "count_errs", "count=$(cat /log/api.log | grep ERROR | wc -l)\necho \"found $count\"")
	if got := strings.TrimSpace(f.run(t, "count_errs").Text); got != "found 2" {
		t.Errorf("stdout = %q, want `found 2`", got)
	}
}

// && short-circuits on exit status, and a successful interceptor call exits 0 so
// the right side runs.
func interiorAndShortCircuit(t *testing.T, f *queryFixture) {
	f.script(t, "and_ok", "cat /log/api.log | grep ERROR | wc -l && echo ok")
	lines := outputLines(t, f.run(t, "and_ok"), 2)
	if lines[0] != "2" || lines[1] != "ok" {
		t.Errorf("got %q, want 2 then ok", lines)
	}
}

// A function body hosts interception too: a command substitution inside it
// captures a query's count, and the function decides on it.
func interiorFunctionConditional(t *testing.T, f *queryFixture) {
	f.script(t, "check_all", `error_check() {
  local count=$(cat /log/$1.log | grep ERROR | wc -l)
  if [ "$count" -gt 1 ]; then
    echo "$1: high ($count)"
    return 1
  fi
  echo "$1: ok ($count)"
  return 0
}
error_check api
error_check web`)
	lines := outputLines(t, f.run(t, "check_all"), 2)
	if lines[0] != "api: high (2)" || lines[1] != "web: ok (1)" {
		t.Errorf("got %q, want `api: high (2)` then `web: ok (1)`", lines)
	}
}

// A function defined by one script is visible to the next: both run on one
// interpreter per request, which is a convenience worth pinning so a scoping
// change cannot land silently.
func interiorFunctionAcrossScripts(t *testing.T, f *queryFixture) {
	f.script(t, "lib", "error_count() {\n  cat /log/$1.log | grep ERROR | wc -l\n}")
	f.script(t, "report", "lib\necho api: $(error_count api)\necho web: $(error_count web)")
	lines := outputLines(t, f.run(t, "report"), 2)
	if lines[0] != "api: 2" || lines[1] != "web: 1" {
		t.Errorf("got %q, want `api: 2` then `web: 1`", lines)
	}
}

// A single-pipeline body still takes the pre-splice path.
func interiorSinglePipeline(t *testing.T, f *queryFixture) {
	f.script(t, "errs", "cat /log/api.log | grep ERROR | wc -l")
	if got := strings.TrimSpace(f.run(t, "errs api").Text); got != "2" {
		t.Errorf("stdout = %q, want 2", got)
	}
}

// A /bin script used as a later stage after a log read runs where /bin exists,
// fed by the query. The query half goes to compute, whose stateless worker has
// no /bin; the script word was once shipped there and failed "compute exit 127:
// command not found". The pipeline is split at the first stage that names one.
func binAfterLogRead(t *testing.T, f *queryFixture) {
	f.script(t, "count", "wc -l")
	if err := f.s.Bins.Save(f.s.Owner, "", "luacount", "local n = 0\nfor _ in lines() do n = n + 1 end\nprint(n)", "lua"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, line, want string }{
		{"bash filter as last stage", `cat /log/api.log | grep ERROR | count`, "2"},
		{"lua filter as last stage", `cat /log/api.log | grep ERROR | luacount`, "2"},
		{"lua filter, whole log", `cat /log/api.log | luacount`, "3"},
		{"stages after the script", `cat /log/api.log | count | awk '{ print $1 + 10 }'`, "13"},
		{"compute stage before the script", `cat /log/api.log | grep -v INFO | sort | count`, "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if d := f.run(t, tc.line); strings.TrimSpace(d.Text) != tc.want {
				t.Errorf("%s\n got %q (stderr %q)\nwant %q", tc.line, strings.TrimSpace(d.Text), d.Stderr, tc.want)
			}
		})
	}
}

// A redirect on the last stage belongs to that stage: with the script split off
// from the query it stays on the script's output, not lifted onto the query.
func binAfterLogReadRedirect(t *testing.T, f *queryFixture) {
	f.script(t, "count", "wc -l")
	if d := f.run(t, `cat /log/api.log | grep ERROR | count > /tmp/n`); strings.TrimSpace(d.Text) != "" || d.Stderr != "" {
		t.Fatalf("redirected output leaked: text %q stderr %q", d.Text, d.Stderr)
	}
	if got := strings.TrimSpace(f.run(t, `cat /tmp/n`).Text); got != "2" {
		t.Errorf("/tmp/n = %q, want the script's output 2", got)
	}
}
