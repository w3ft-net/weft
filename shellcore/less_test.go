package shellcore

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// lessContent renders the current viewport of the pager as a
// newline-joined string with ANSI sequences stripped. Used for
// assertions that test *what* the user is seeing rather than the
// exact escape-sequence wire format.
func lessContent(c *ShellCore) string {
	l := c.less
	if l == nil {
		return ""
	}
	height := l.height - 1
	if height < 1 {
		height = 1
	}
	end := l.offset + height
	if end > len(l.lines) {
		end = len(l.lines)
	}
	var b strings.Builder
	for i := l.offset; i < end; i++ {
		b.WriteString(stripANSI(l.lines[i]))
		b.WriteByte('\n')
	}
	return b.String()
}

// enterLessWithContent builds a core and drops straight into the
// pager with n lines, useful for navigation tests.
func enterLessWithContent(t *testing.T, n int) *ShellCore {
	t.Helper()
	core, _ := newTestCore([]string{})
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	core.enterLess(sb.String(), "test")
	if core.Mode() != ModeLess {
		t.Fatalf("expected ModeLess, got %d", core.Mode())
	}
	return core
}

func TestLess_EnterAndExitOnQ(t *testing.T) {
	core := enterLessWithContent(t, 100)
	core.HandleEvent(KeyEvent{Rune: 'q'})
	if core.Mode() != ModeInput {
		t.Errorf("mode after q = %d, want ModeInput", core.Mode())
	}
	if core.less != nil {
		t.Error("less state should be nil after exit")
	}
}

func TestLess_ExitOnCtrlC(t *testing.T) {
	core := enterLessWithContent(t, 100)
	core.HandleEvent(KeyEvent{Key: KeyCtrlC})
	if core.Mode() != ModeInput {
		t.Errorf("mode after Ctrl+C = %d, want ModeInput", core.Mode())
	}
}

func TestLess_PageDownAdvancesByViewport(t *testing.T) {
	core := enterLessWithContent(t, 100)
	// Viewport is 24-1=23 content rows, pageStep = 23-1 = 22.
	before := lessContent(core)
	if !strings.Contains(before, "line 1\n") {
		t.Fatalf("expected top to start at line 1: %q", before)
	}
	core.HandleEvent(KeyEvent{Rune: ' '})
	after := lessContent(core)
	if strings.Contains(after, "line 1\n") {
		t.Errorf("after page down, viewport should not contain line 1: %q", after)
	}
	if !strings.Contains(after, "line 23\n") {
		t.Errorf("after page down, expected line 23 visible: %q", after)
	}
}

func TestLess_GJumpsToTop_GCapJumpsToBottom(t *testing.T) {
	core := enterLessWithContent(t, 200)
	core.HandleEvent(KeyEvent{Rune: ' '})
	core.HandleEvent(KeyEvent{Rune: ' '}) // scroll down a bit
	core.HandleEvent(KeyEvent{Rune: 'G'})
	if !strings.Contains(lessContent(core), "line 200\n") {
		t.Errorf("G should land on last line: %q", lessContent(core))
	}
	core.HandleEvent(KeyEvent{Rune: 'g'})
	if !strings.HasPrefix(lessContent(core), "line 1\n") {
		t.Errorf("g should return to top: %q", lessContent(core))
	}
}

func TestLess_JAndKStepByOneLine(t *testing.T) {
	core := enterLessWithContent(t, 100)
	core.HandleEvent(KeyEvent{Rune: 'j'})
	if core.less.offset != 1 {
		t.Errorf("offset after j = %d, want 1", core.less.offset)
	}
	core.HandleEvent(KeyEvent{Rune: 'k'})
	if core.less.offset != 0 {
		t.Errorf("offset after k = %d, want 0", core.less.offset)
	}
}

func TestLess_ClampsAtBothEnds(t *testing.T) {
	// 10 lines in a 24-high viewport — the whole buffer fits. Offset
	// should stay at 0 no matter how hard we scroll.
	core, _ := newTestCore([]string{})
	core.enterLess("a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n", "short")
	core.HandleEvent(KeyEvent{Rune: ' '})
	core.HandleEvent(KeyEvent{Rune: ' '})
	if core.less.offset != 0 {
		t.Errorf("short buffer should clamp offset to 0, got %d", core.less.offset)
	}
	core.HandleEvent(KeyEvent{Rune: 'k'})
	if core.less.offset != 0 {
		t.Errorf("k at top should stay at 0, got %d", core.less.offset)
	}
}

func TestLess_SearchJumpsToMatch(t *testing.T) {
	core := enterLessWithContent(t, 100)
	core.HandleEvent(KeyEvent{Rune: '/'})
	for _, r := range "line 42" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})
	if !strings.Contains(lessContent(core), "line 42\n") {
		t.Errorf("search should jump to line 42: %q", lessContent(core))
	}
}

func TestLess_NAndShiftNCycleMatches(t *testing.T) {
	core, _ := newTestCore([]string{})
	var sb strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
		if i%10 == 0 {
			sb.WriteString("TARGET marker\n")
		}
	}
	core.enterLess(sb.String(), "search")
	// Search: /TARGET
	core.HandleEvent(KeyEvent{Rune: '/'})
	for _, r := range "TARGET" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})

	firstOffset := core.less.offset
	core.HandleEvent(KeyEvent{Rune: 'n'})
	if core.less.offset == firstOffset {
		t.Errorf("n should advance to next TARGET, offset stayed at %d", firstOffset)
	}
	core.HandleEvent(KeyEvent{Rune: 'N'})
	if core.less.offset != firstOffset {
		t.Errorf("N after n should return to first match %d, got %d", firstOffset, core.less.offset)
	}
}

func TestLess_SearchNotFoundStatus(t *testing.T) {
	core := enterLessWithContent(t, 50)
	core.HandleEvent(KeyEvent{Rune: '/'})
	for _, r := range "zzzz" {
		core.HandleEvent(KeyEvent{Rune: r})
	}
	core.HandleEvent(KeyEvent{Key: KeyEnter})
	if !strings.Contains(core.less.status, "not found") {
		t.Errorf("expected 'not found' status, got %q", core.less.status)
	}
}

// --- Help topic dispatch ---

type fakeTiers struct{}

func (fakeTiers) WatchLimit(tier string) int {
	switch tier {
	case "free":
		return 0
	case "pro":
		return 10
	}
	return 0
}

func (fakeTiers) MinWatchInterval(tier string) time.Duration {
	switch tier {
	case "free":
		return 0
	case "pro":
		return time.Minute
	}
	return 0
}

func (fakeTiers) Tiers() []string { return []string{"free", "pro"} }

func TestHelp_IndexIsShortAndLinksTopics(t *testing.T) {
	text, ok := (&ShellCore{}).topicDispatch("help")
	if !ok {
		t.Fatal("help not dispatched")
	}
	stripped := stripANSI(text)
	lines := strings.Count(stripped, "\n")
	if lines > 14 {
		t.Errorf("help index should be short, got %d lines:\n%s", lines, stripped)
	}
	for _, want := range []string{"help builtins", "help cron", "help alerts", "man alert", "man less"} {
		if !strings.Contains(stripped, want) {
			t.Errorf("help index missing %q", want)
		}
	}
}

func TestHelp_CronRendersTierLimitsFromProvider(t *testing.T) {
	c := &ShellCore{}
	c.SetTierLimits(fakeTiers{})
	text, ok := c.topicDispatch("help cron")
	if !ok {
		t.Fatal("help cron not dispatched")
	}
	stripped := stripANSI(text)
	// Free tier shows "no watches"; pro shows "10 watches" + "min 1m interval".
	if !strings.Contains(stripped, "no watches") {
		t.Errorf("free tier row missing 'no watches' in:\n%s", stripped)
	}
	if !strings.Contains(stripped, "10 watches") {
		t.Errorf("pro tier row missing '10 watches' in:\n%s", stripped)
	}
	if !strings.Contains(stripped, "min 1m interval") {
		t.Errorf("pro tier row missing 'min 1m interval' in:\n%s", stripped)
	}
}

func TestHelp_AlertsMentionsNotifyConf(t *testing.T) {
	text, ok := (&ShellCore{}).topicDispatch("help alerts")
	if !ok {
		t.Fatal("help alerts not dispatched")
	}
	stripped := stripANSI(text)
	for _, want := range []string{"/etc/notify.conf", "--notify=", "vi /etc/notify.conf", "above", "rising"} {
		if !strings.Contains(stripped, want) {
			t.Errorf("help alerts missing %q in:\n%s", want, stripped)
		}
	}
}

func TestHelp_ManAliases(t *testing.T) {
	cases := []string{"man alert", "alert --help", "man range", "range --help", "man less", "less --help"}
	for _, c := range cases {
		if _, ok := (&ShellCore{}).topicDispatch(c); !ok {
			t.Errorf("%q should be dispatched", c)
		}
	}
}

// Auto-pager integration: long help goes through ModeLess, short help
// does not. We stimulate the user-facing entry point (execBuiltin) so
// the test catches regressions in showDoc's threshold logic too.
func TestHelp_AutoPagesLongContent(t *testing.T) {
	core, _ := newTestCore([]string{})
	core.SetTierLimits(fakeTiers{})
	// help cron is the longest topic — over 30 lines after tier rendering.
	core.HandleEvent(KeyEvent{Rune: 'h'})
	core.HandleEvent(KeyEvent{Rune: 'e'})
	core.HandleEvent(KeyEvent{Rune: 'l'})
	core.HandleEvent(KeyEvent{Rune: 'p'})
	core.HandleEvent(KeyEvent{Rune: ' '})
	core.HandleEvent(KeyEvent{Rune: 'c'})
	core.HandleEvent(KeyEvent{Rune: 'r'})
	core.HandleEvent(KeyEvent{Rune: 'o'})
	core.HandleEvent(KeyEvent{Rune: 'n'})
	core.HandleEvent(KeyEvent{Key: KeyEnter})
	if core.Mode() != ModeLess {
		t.Errorf("help cron should enter pager, mode=%d", core.Mode())
	}
}

func TestHelp_ShortIndexStaysInline(t *testing.T) {
	core, h := newTestCore([]string{})
	core.HandleEvent(KeyEvent{Rune: 'h'})
	core.HandleEvent(KeyEvent{Rune: 'e'})
	core.HandleEvent(KeyEvent{Rune: 'l'})
	core.HandleEvent(KeyEvent{Rune: 'p'})
	core.HandleEvent(KeyEvent{Key: KeyEnter})
	if core.Mode() != ModeInput {
		t.Errorf("short help should stay inline, mode=%d", core.Mode())
	}
	if !strings.Contains(h.Scrollback(), "help builtins") {
		t.Errorf("expected help index in scrollback:\n%s", h.Scrollback())
	}
}

// Piping `| less` routes into ModeLess when the query result returns.
func TestLess_PendingFlagRoutesResultIntoPager(t *testing.T) {
	core, _ := newTestCore([]string{"api"})
	// Simulate the user typing "cat /log/api.log | less" and pressing Enter.
	core.startQuery("cat /log/api.log | less")
	if !core.pendingLess {
		t.Fatal("startQuery should have flagged pendingLess")
	}
	// Result arrives (as if from the coordinator).
	core.HandleEvent(QueryResultEvent{Text: "some\nresult\nlines\n"})
	if core.Mode() != ModeLess {
		t.Errorf("expected ModeLess after `| less` result, got %d", core.Mode())
	}
	if core.pendingLess {
		t.Error("pendingLess should be cleared after consumption")
	}
}

func TestLess_ErrorStillClearsPendingFlag(t *testing.T) {
	core, _ := newTestCore([]string{"api"})
	core.startQuery("cat /log/api.log | less")
	core.HandleEvent(QueryResultEvent{Err: fmtErr("no such source")})
	if core.pendingLess {
		t.Error("pendingLess should be cleared on error")
	}
	if core.Mode() != ModeInput {
		t.Errorf("error should leave mode at ModeInput, got %d", core.Mode())
	}
}

func fmtErr(msg string) error { return fmt.Errorf("%s", msg) }
