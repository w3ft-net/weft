package mcp

import (
	"fmt"
	"strings"
	"testing"
)

// TestLs_HappyPath: a /log path returns its entries as plain text,
// one per line. The model uses this to discover what apps exist
// before drafting a pipeline.
func TestLs_HappyPath(t *testing.T) {
	s := testSession(t, Config{
		Run: neverRunFn(t),
		Ls: func(path string) ([]string, error) {
			if path != "/log/billing" {
				t.Errorf("got path %q, want /log/billing", path)
			}
			return []string{"billing.log", "billing-debug.log"}, nil
		},
	})

	res := callTool(t, s, "weft.ls", map[string]any{"path": "/log/billing"})
	if res.IsError {
		t.Fatalf("unexpected IsError; content=%q", textContent(res))
	}
	got := textContent(res)
	for _, want := range []string{"billing.log", "billing-debug.log"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in output; got %q", want, got)
		}
	}
}

// TestLs_RejectOutsideLog: anything not under /log/ is rejected at
// the path-scope check. The LsFunc must never be called.
func TestLs_RejectOutsideLog(t *testing.T) {
	cases := []string{
		"/etc/notify.yaml",
		"/bin/billing-job",
		"/cron.d/nightly",
		"/log/../etc",
		"etc",       // relative
		"../../etc", // dotdot
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			s := testSession(t, Config{
				Run: neverRunFn(t),
				Ls: func(_ string) ([]string, error) {
					t.Errorf("LsFunc must not be called for forbidden path %q", p)
					return nil, nil
				},
			})
			res := callTool(t, s, "weft.ls", map[string]any{"path": p})
			if !res.IsError {
				t.Errorf("expected IsError for path %q", p)
			}
		})
	}
}

// TestLs_AllowsLogRoot: the "/log" and "/log/" forms must be
// reachable so the model can list namespaces from the top.
func TestLs_AllowsLogRoot(t *testing.T) {
	for _, p := range []string{"/log", "/log/"} {
		t.Run(p, func(t *testing.T) {
			s := testSession(t, Config{
				Run: neverRunFn(t),
				Ls: func(_ string) ([]string, error) {
					return []string{"billing", "vimgym"}, nil
				},
			})
			res := callTool(t, s, "weft.ls", map[string]any{"path": p})
			if res.IsError {
				t.Errorf("expected success for %q; got %q", p, textContent(res))
			}
			if !strings.Contains(textContent(res), "billing") {
				t.Errorf("expected entries in output; got %q", textContent(res))
			}
		})
	}
}

// TestCat_HappyPath: file contents are returned wrapped in the
// same <weft-data> sentinel as weft.run output, because the
// content is also untrusted-by-default log data.
func TestCat_HappyPath(t *testing.T) {
	body := "2026-04-28T12:00:00Z level=ERROR msg=oops\n2026-04-28T12:00:01Z level=INFO msg=ok\n"
	s := testSession(t, Config{
		Run: neverRunFn(t),
		Cat: func(path string) ([]byte, error) {
			if path != "/log/billing/billing.log" {
				t.Errorf("got path %q, want /log/billing/billing.log", path)
			}
			return []byte(body), nil
		},
	})

	res := callTool(t, s, "weft.cat", map[string]any{"path": "/log/billing/billing.log"})
	if res.IsError {
		t.Fatalf("unexpected IsError; content=%q", textContent(res))
	}
	got := textContent(res)
	if !strings.Contains(got, "<weft-data>") {
		t.Errorf("expected sentinel-wrapped output; got %q", got)
	}
	if !strings.Contains(got, "level=ERROR") {
		t.Errorf("expected file body in output; got %q", got)
	}
}

// TestCat_RejectOutsideLog mirrors TestLs_RejectOutsideLog: the
// path-scope check is the same boundary for both file readers.
func TestCat_RejectOutsideLog(t *testing.T) {
	cases := []string{
		"/etc/notify.yaml",
		"/bin/billing-job",
		"/cron.d/nightly",
		"/log/../etc/notify.yaml",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			s := testSession(t, Config{
				Run: neverRunFn(t),
				Cat: func(_ string) ([]byte, error) {
					t.Errorf("CatFunc must not be called for forbidden path %q", p)
					return nil, nil
				},
			})
			res := callTool(t, s, "weft.cat", map[string]any{"path": p})
			if !res.IsError {
				t.Errorf("expected IsError for path %q", p)
			}
		})
	}
}

// TestCat_OutputCapped: a giant log file is capped the same way
// weft.run output is capped — sharing capWriter behaviour means the
// model never gets blasted with megabytes of content.
func TestCat_OutputCapped(t *testing.T) {
	s := testSession(t, Config{
		Run: neverRunFn(t),
		Cat: func(_ string) ([]byte, error) {
			var b strings.Builder
			for i := 0; i < 1000; i++ {
				fmt.Fprintf(&b, "row %d\n", i)
			}
			return []byte(b.String()), nil
		},
		MaxRows: 50,
	})

	res := callTool(t, s, "weft.cat", map[string]any{"path": "/log/x/x.log"})
	got := textContent(res)
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected truncation notice; got %q", lastN(got, 200))
	}
}

// TestCat_FSErrorSurfaced: an underlying I/O error reaches the
// model as an IsError result with the underlying message.
func TestCat_FSErrorSurfaced(t *testing.T) {
	s := testSession(t, Config{
		Run: neverRunFn(t),
		Cat: func(_ string) ([]byte, error) {
			return nil, fmt.Errorf("file does not exist")
		},
	})
	res := callTool(t, s, "weft.cat", map[string]any{"path": "/log/x/missing.log"})
	if !res.IsError {
		t.Fatalf("expected IsError")
	}
	if !strings.Contains(textContent(res), "file does not exist") {
		t.Errorf("expected underlying error in content; got %q", textContent(res))
	}
}
