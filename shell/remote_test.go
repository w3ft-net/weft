package shell

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
)

// remoteCore builds a core in the configuration the CLI and the browser use:
// ConfigureCore with a Query/Follow, which installs the remote execution path.
func remoteCore(t *testing.T, s *Shell) *shellcore.Shell {
	t.Helper()
	return remoteCoreWith(t, s, nil)
}

// remoteCoreWith is remoteCore with post, when non-nil, running each line on
// its own goroutine and returning through it.
func remoteCoreWith(t *testing.T, s *Shell, post func(func())) *shellcore.Shell {
	t.Helper()
	if s.FS == nil {
		s.FS = testMountFS(nil)
	}
	var core *shellcore.Shell
	if post != nil {
		core = newCoreAsync(80, 24, func([]byte) {}, func(shellcore.Action) {}, post)
	} else {
		core = newCoreD(80, 24, func([]byte) {}, func(shellcore.Action) {})
	}
	s.ConfigureCore(core)
	return core
}

func enter(core *shellcore.Shell, line string) {
	for _, r := range line {
		core.HandleEvent(shellcore.KeyEvent{Rune: r})
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyEnter})
}

func screen(core *shellcore.Shell) string {
	return strings.ReplaceAll(shellcore.StripANSI(core.Scrollback()), "\r\n", "\n")
}

type recorder struct {
	mu      sync.Mutex
	lines   []string
	stacks  [][]string
	ranges  []string
	result  *QueryResult
	err     error
	release chan struct{} // when non-nil, Query blocks until it is closed
}

func (r *recorder) query(pipeline, rng string, stack []string) (*QueryResult, error) {
	r.mu.Lock()
	r.lines = append(r.lines, pipeline)
	r.stacks = append(r.stacks, stack)
	r.ranges = append(r.ranges, rng)
	rel := r.release
	r.mu.Unlock()
	if rel != nil {
		<-rel
	}
	return r.result, r.err
}

func TestRemote_ShipsTheLineVerbatimAndPrintsTheResult(t *testing.T) {
	rec := &recorder{result: &QueryResult{Text: "hello from server\n"}}
	core := remoteCore(t, &Shell{Query: rec.query, Range: "1d.."})
	const line = "cat   api.log |   head -1"
	enter(core, line)
	if len(rec.lines) != 1 || rec.lines[0] != line {
		t.Fatalf("shipped %q, want %q verbatim", rec.lines, line)
	}
	if rec.ranges[0] != "1d.." {
		t.Errorf("range = %q", rec.ranges[0])
	}
	if !strings.Contains(screen(core), "hello from server") {
		t.Errorf("screen = %q", screen(core))
	}
}

// cd is the shell's own, so it runs on the client and only the server's part of
// the line ships, with the directory stack as the cd left it.
func TestRemote_CdRunsLocallyAndTheStackTravels(t *testing.T) {
	rec := &recorder{result: &QueryResult{Text: "ok\n"}}
	core := remoteCore(t, &Shell{Query: rec.query})
	enter(core, "cd /log/acme &&   cat api.log | head -1")
	if len(rec.lines) != 1 || rec.lines[0] != "cat api.log | head -1" {
		t.Fatalf("shipped %q", rec.lines)
	}
	if got := rec.stacks[0]; len(got) == 0 || got[0] != "/log/acme" {
		t.Errorf("shipped stack = %v, want cwd /log/acme", got)
	}
}

// A client stage may sit anywhere in a line, not only at its end.
func TestRemote_ClientStageMixesWithServerCommands(t *testing.T) {
	rec := &recorder{result: &QueryResult{Text: "row\n"}}
	core := remoteCore(t, &Shell{Query: rec.query})
	core.RegisterCommand("range", func(*shellcore.Cmd) int { return 0 })
	enter(core, "range 1h; cat 'a|b' | head -1")
	if len(rec.lines) != 1 || rec.lines[0] != "cat 'a|b' | head -1" {
		t.Fatalf("shipped %q, want the quoted pipe kept inside one segment", rec.lines)
	}
}

func TestRemote_RowsAreJoinedWithNewlines(t *testing.T) {
	rec := &recorder{result: &QueryResult{Rows: []Row{{Raw: "a"}, {Raw: "b"}}}}
	core := remoteCore(t, &Shell{Query: rec.query})
	enter(core, "cat x")
	if !strings.Contains(screen(core), "a\nb\n") {
		t.Errorf("screen = %q", screen(core))
	}
}

func TestRemote_ShipsTheDirStackAndAppliesTheReturnedOne(t *testing.T) {
	rec := &recorder{result: &QueryResult{Text: "ok\n", FinalDirStack: []string{"/log/acme", "/"}}}
	core := remoteCore(t, &Shell{Query: rec.query})
	core.SetDirStack([]string{"/log", "/"})
	enter(core, "ls acme")
	if got := rec.stacks[0]; len(got) != 2 || got[0] != "/log" {
		t.Errorf("shipped stack = %v", got)
	}
	if got := core.DirStack(); got[0] != "/log/acme" {
		t.Errorf("DirStack after = %v, want it to follow the server", got)
	}
}

func TestRemote_ErrorIsShownAndExitsNonZero(t *testing.T) {
	rec := &recorder{err: errors.New("query: no such file: main.log")}
	core := remoteCore(t, &Shell{Query: rec.query})
	done := shellcore.Subscribe[shellcore.CommandCompleted](core)
	enter(core, "cat main.log")
	if !strings.Contains(screen(core), "no such file: main.log") {
		t.Errorf("screen = %q", screen(core))
	}
	if ev := <-done; ev.ExitCode == 0 {
		t.Error("exit code 0 for a failed query")
	}
}

func TestRemote_NoticesPrecedeOutput(t *testing.T) {
	rec := &recorder{result: &QueryResult{
		Text:    "body\n",
		Notices: []QueryResultNotice{{Kind: "warning", Title: "Heads up", Body: "tmp expires"}},
	}}
	core := remoteCore(t, &Shell{Query: rec.query})
	enter(core, "cat x")
	out := screen(core)
	i, j := strings.Index(out, "Heads up"), strings.Index(out, "body")
	if i < 0 || j < i {
		t.Errorf("screen = %q", out)
	}
}

func TestRemote_ClientBuiltinsDoNotShip(t *testing.T) {
	rec := &recorder{result: &QueryResult{Text: "x"}}
	core := remoteCore(t, &Shell{Query: rec.query, Range: "1d.."})
	enter(core, "range 1h")
	enter(core, "history")
	if len(rec.lines) != 0 {
		t.Errorf("client builtins were shipped: %q", rec.lines)
	}
}

// A trailing client-side stage runs locally; only what precedes it ships, as
// typed.
func TestRemote_TrailingChartStageStaysLocal(t *testing.T) {
	rec := &recorder{result: &QueryResult{Text: "2026-10-10T12:00:01Z ERROR a\n2026-10-10T12:07:00Z ERROR b\n"}}
	var got []ChartRequest
	core := remoteCore(t, &Shell{Query: rec.query, Chart: func(r ChartRequest) { got = append(got, r) }})
	enter(core, "cat main.log | grep ERROR | chart -x 1 --bucket 5m")
	if len(rec.lines) != 1 || rec.lines[0] != "cat main.log | grep ERROR" {
		t.Errorf("shipped %q", rec.lines)
	}
	if len(got) != 1 || got[0].Pipeline != "cat main.log | grep ERROR | chart -x 1 --bucket 5m" {
		t.Errorf("chart requests = %+v", got)
	}
}

func TestRemote_LessPagesLocally(t *testing.T) {
	rec := &recorder{result: &QueryResult{Text: "one\ntwo\n"}}
	core := remoteCore(t, &Shell{Query: rec.query})
	enter(core, "cat x | less")
	if rec.lines[0] != "cat x" {
		t.Errorf("shipped %q", rec.lines)
	}
	if core.Mode() != shellcore.ModeLess {
		t.Errorf("mode = %v, want ModeLess", core.Mode())
	}
}

func TestRemote_LessFileForm(t *testing.T) {
	rec := &recorder{result: &QueryResult{Text: "one\n"}}
	core := remoteCore(t, &Shell{Query: rec.query})
	enter(core, "less /log/x")
	if rec.lines[0] != "cat /log/x" || core.Mode() != shellcore.ModeLess {
		t.Errorf("shipped %q mode %v", rec.lines, core.Mode())
	}
}

// asyncRemoteCore runs lines on the core's own goroutine, with a queue
// standing in for the embedder's event loop.
func asyncRemoteCore(t *testing.T, s *Shell) (*shellcore.Shell, chan func()) {
	t.Helper()
	q := make(chan func(), 1024)
	core := remoteCoreWith(t, s, func(fn func()) { q <- fn })
	return core, q
}

func pump(t *testing.T, q chan func(), done func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !done() {
		select {
		case fn := <-q:
			fn()
		case <-deadline:
			t.Fatal("timed out")
		}
	}
}

func TestRemote_FollowStreamsRowsUntilCancelled(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	s := &Shell{
		Query: (&recorder{}).query,
		Follow: func(ctx context.Context, pipeline, _ string, onRow func(Row)) error {
			close(started)
			onRow(Row{Raw: "row-1"})
			onRow(Row{Raw: "row-2"})
			<-ctx.Done()
			close(stopped)
			return ctx.Err()
		},
	}
	core, q := asyncRemoteCore(t, s)
	enter(core, "tail -f /log/x")
	<-started
	pump(t, q, func() bool { return strings.Contains(screen(core), "row-2") })
	if !strings.Contains(screen(core), "Ctrl+C to stop") {
		t.Error("no streaming hint")
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyCtrlC})
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Ctrl+C did not stop the follow")
	}
	if core.Mode() != shellcore.ModeInput {
		t.Errorf("mode = %v", core.Mode())
	}
}

// Ctrl+C frees the run even though Query itself cannot be interrupted, and
// the late reply is dropped.
func TestRemote_CtrlCAbandonsASlowQuery(t *testing.T) {
	rec := &recorder{result: &QueryResult{Text: "LATE\n"}, release: make(chan struct{})}
	core, q := asyncRemoteCore(t, &Shell{Query: rec.query})
	enter(core, "cat x")
	for {
		rec.mu.Lock()
		n := len(rec.lines)
		rec.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyCtrlC})
	close(rec.release) // the server finally answers
	time.Sleep(50 * time.Millisecond)
	for len(q) > 0 {
		(<-q)()
	}
	if strings.Contains(screen(core), "LATE") {
		t.Error("a cancelled query's reply reached the screen")
	}
}

// A server that returns no dirstack (a non-cd pipeline, or the in-process dev
// shell) must not blank the prompt's cwd.
func TestRemote_NoDirStackLeavesCwd(t *testing.T) {
	rec := &recorder{result: &QueryResult{Text: "hi\n"}} // no FinalDirStack
	core := remoteCore(t, &Shell{Query: rec.query})
	core.SetDirStack([]string{"/log/acme"})
	enter(core, "cat api.log")
	if got := core.DirStack(); len(got) != 1 || got[0] != "/log/acme" {
		t.Errorf("DirStack = %v, want [/log/acme] preserved", got)
	}
}

// Ctrl+C reaches a query that is a pipeline stage (a trailing client stage
// such as `| less` makes the callback one), not just a bare query.
func TestRemote_CtrlCReachesAQueryInsideAPipeline(t *testing.T) {
	started := make(chan struct{})
	freed := make(chan struct{})
	s := &Shell{QueryCtx: func(ctx context.Context, pipeline, _ string, _ []string) (*QueryResult, error) {
		close(started)
		<-ctx.Done()
		close(freed)
		return nil, ctx.Err()
	}}
	core, q := asyncRemoteCore(t, s)
	enter(core, "cat x | less")
	<-started
	core.HandleEvent(shellcore.KeyEvent{Key: shellcore.KeyCtrlC})
	select {
	case <-freed:
	case <-time.After(3 * time.Second):
		t.Fatal("the query's context was not cancelled")
	}
	for len(q) > 0 {
		(<-q)()
	}
}

// `trace` is a client command only for the shapes the browser has a modal for.
// Every other shape is yielded: it ships to the server like any other line.
func TestRemote_TraceClaimsOnlyWhatTheBrowserCanShow(t *testing.T) {
	var opened []TraceRequest
	rec := &recorder{result: &QueryResult{Text: "from-server\n"}}
	core := remoteCore(t, &Shell{Query: rec.query, Trace: func(r TraceRequest) { opened = append(opened, r) }})

	for _, line := range []string{"trace --id abc123", "trace --spans", "trace"} {
		enter(core, line)
	}
	if len(opened) != 3 || len(rec.lines) != 0 {
		t.Fatalf("claimed shapes: opened %d modals, shipped %q; want 3 and none", len(opened), rec.lines)
	}

	for _, line := range []string{"trace --spans --name x --samples", "trace --bogus", "trace | jq .lines"} {
		enter(core, line)
	}
	if len(opened) != 3 {
		t.Errorf("yielded shapes opened modals: %+v", opened[3:])
	}
	want := []string{"trace --spans --name x --samples", "trace --bogus", "trace | jq .lines"}
	if len(rec.lines) != 3 || rec.lines[0] != want[0] || rec.lines[1] != want[1] || rec.lines[2] != want[2] {
		t.Errorf("shipped %q, want %q verbatim", rec.lines, want)
	}
}

func TestRemote_TraceModalCarriesTheNormalisedPipeline(t *testing.T) {
	var opened []TraceRequest
	core := remoteCore(t, &Shell{Query: (&recorder{result: &QueryResult{}}).query, Trace: func(r TraceRequest) { opened = append(opened, r) }})
	enter(core, "trace   --id   abc123")
	if len(opened) != 1 || opened[0].Pipeline != "trace --id abc123" || opened[0].Mode != "gap" {
		t.Errorf("opened = %+v", opened)
	}
}
