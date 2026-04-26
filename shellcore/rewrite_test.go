package shellcore

import (
	"strings"
	"testing"
)

func TestRewritePathsAndChain(t *testing.T) {
	got, err := RewritePaths(`cd /log/acme && cat api.log | head -1`, NewTransientDirStack())
	if err != nil {
		t.Fatalf("RewritePaths: %v", err)
	}
	if !strings.Contains(got, "cat /log/acme/api.log") {
		t.Errorf("got %q, want cat /log/acme/api.log to appear", got)
	}
}

func TestRewritePathsSemicolon(t *testing.T) {
	got, _ := RewritePaths(`cd /log/acme; tail -f api.log`, NewTransientDirStack())
	if !strings.Contains(got, "tail -f /log/acme/api.log") {
		t.Errorf("got %q, want tail -f /log/acme/api.log to appear", got)
	}
}

func TestRewritePathsRelativeCdChain(t *testing.T) {
	got, _ := RewritePaths(`cd /log && cd acme && cat api.log`, NewTransientDirStack())
	if !strings.Contains(got, "cat /log/acme/api.log") {
		t.Errorf("got %q, want cat /log/acme/api.log to appear", got)
	}
}

func TestRewritePathsAbsoluteUnchanged(t *testing.T) {
	got, _ := RewritePaths(`cd /log/acme && cat /log/other/x.log`, NewTransientDirStack())
	if !strings.Contains(got, "cat /log/other/x.log") {
		t.Errorf("absolute path mutated: %q", got)
	}
}

func TestRewritePathsNonQueryUnchanged(t *testing.T) {
	// /etc paths classify as PathLocal, not PathQuery — must not be
	// rewritten with cwd.
	got, _ := RewritePaths(`cd /log/acme && cat /etc/hostname`, NewTransientDirStack())
	if !strings.Contains(got, "cat /etc/hostname") {
		t.Errorf("/etc path mutated: %q", got)
	}
}

func TestRewritePathsFlagsUnchanged(t *testing.T) {
	got, _ := RewritePaths(`cd /log && tail -1 api.log`, NewTransientDirStack())
	if !strings.Contains(got, "tail -1 /log/api.log") {
		t.Errorf("got %q, want tail -1 /log/api.log to appear", got)
	}
	if strings.Contains(got, "/log/-1") {
		t.Errorf("flag was incorrectly rewritten: %q", got)
	}
}

