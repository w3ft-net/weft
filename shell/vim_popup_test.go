package shell

import (
	"context"
	"errors"
	warpfs "github.com/graybeardcorp/warp/pkg/fs"
	"github.com/w3ft-net/weft/warning"
	"io/fs"
	"strings"
	"testing"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
)

// stubPopupFS is a minimal in-memory FS that satisfies warpfs.FS
// and exposes an AmendVersionMessage hook for the popup tests.
// Independent of weft's real ShellFS so the tests don't need Postgres.
type stubPopupFS struct {
	files      map[string]string
	amendCalls []amendCall
	// writeErr, when set, is returned by the next WriteFile call
	// instead of nil — tests use this to simulate a real save
	// surface's warning.Warning (or a genuine blocking error).
	writeErr error
}

type amendCall struct{ path, message string }

func newStubPopupFS() *stubPopupFS {
	return &stubPopupFS{files: map[string]string{}}
}

func (f *stubPopupFS) ReadFile(ctx context.Context, p string) ([]byte, error) {
	if v, ok := f.files[p]; ok {
		return []byte(v), nil
	}
	return nil, nil
}

func (f *stubPopupFS) WriteFile(ctx context.Context, p string, d []byte) error {
	f.files[p] = string(d)
	return f.writeErr
}

func (f *stubPopupFS) ReadDir(ctx context.Context, p string) ([]fs.DirEntry, error) { return nil, nil }
func (f *stubPopupFS) Stat(ctx context.Context, p string) (fs.FileInfo, error) {
	return nil, fs.ErrNotExist
}
func (f *stubPopupFS) Remove(ctx context.Context, p string) error { return nil }

func (f *stubPopupFS) AmendVersionMessage(path, message string) error {
	f.amendCalls = append(f.amendCalls, amendCall{path, message})
	return nil
}

// newConfiguredCore builds a ShellCore wired through Shell.ConfigureCore
// with a stub FS, so the popup-flow tests exercise the same code path
// production weft uses.
func newConfiguredCore(t *testing.T, fs warpfs.FS) *shellcore.Shell {
	t.Helper()
	core := newCoreD(120, 40,
		func(p []byte) {},
		func(shellcore.Action) {},
	)
	(&Shell{User: "alice", FS: fs}).ConfigureCore(core)
	return core
}

// TestVimPopup_SaveOnBin_OpensMessagePopup pins the post-save commit-message
// popup installed through weft's VimHooks: :wq on /bin/scan re-enters ModeVim
// with a message-prefixed buffer rather than returning to the prompt.
func TestVimPopup_SaveOnBin_OpensMessagePopup(t *testing.T) {
	fs := newStubPopupFS()
	c := newConfiguredCore(t, fs)

	c.StartVim("/bin/scan")
	ed := c.VimEditor()
	if ed == nil {
		t.Fatal("StartVim did not produce an editor")
	}
	if err := ed.Hooks.OnSave("echo hi"); err != nil {
		t.Fatalf("OnSave: %v", err)
	}
	ed.Hooks.OnQuit(false)

	if c.Mode() != shellcore.ModeVim {
		t.Fatalf("after :wq on /bin: expected ModeVim (popup), got %v", c.Mode())
	}
	popup := c.VimEditor()
	if popup == nil {
		t.Fatal("popup editor not active")
	}
	body := popup.Buffer.String()
	if !strings.Contains(body, "# Commit message for /bin/scan") {
		t.Errorf("popup buffer should mention the saved path; got:\n%s", body)
	}
	if !strings.Contains(body, "Save & exit") {
		t.Errorf("popup buffer should explain save/quit semantics; got:\n%s", body)
	}
}

// TestVimPopup_SaveOnTmp_NoPopup: /tmp is not a versioned namespace,
// so :wq should return to ModeInput without opening the popup.
func TestVimPopup_SaveOnTmp_NoPopup(t *testing.T) {
	fs := newStubPopupFS()
	c := newConfiguredCore(t, fs)

	c.StartVim("/tmp/notes")
	ed := c.VimEditor()
	if ed == nil {
		t.Fatal("StartVim did not produce an editor")
	}
	ed.Hooks.OnSave("scratch")
	ed.Hooks.OnQuit(false)

	if c.Mode() != shellcore.ModeInput {
		t.Errorf("after :wq on /tmp: expected ModeInput, got %v", c.Mode())
	}
	if c.VimEditor() != nil {
		t.Error("after :wq on /tmp: expected no active vim editor")
	}
}

// TestVimPopup_SaveOnCronD_OpensPopup: parallel coverage for /cron.d.
func TestVimPopup_SaveOnCronD_OpensPopup(t *testing.T) {
	fs := newStubPopupFS()
	c := newConfiguredCore(t, fs)

	c.StartVim("/cron.d/errors")
	ed := c.VimEditor()
	ed.Hooks.OnSave("# schedule: every 1m\necho 1")
	ed.Hooks.OnQuit(false)

	if c.Mode() != shellcore.ModeVim {
		t.Fatalf("after :wq on /cron.d: expected ModeVim (popup); got %v", c.Mode())
	}
	popup := c.VimEditor()
	if !strings.Contains(popup.Buffer.String(), "# Commit message for /cron.d/errors") {
		t.Errorf("popup buffer should mention path; got:\n%s", popup.Buffer.String())
	}
}

// TestVimPopup_MessageRecordsAmend: typing a message and :wq in the
// popup calls AmendVersionMessage with the stripped content.
func TestVimPopup_MessageRecordsAmend(t *testing.T) {
	fs := newStubPopupFS()
	c := newConfiguredCore(t, fs)

	c.StartVim("/bin/scan")
	ed := c.VimEditor()
	ed.Hooks.OnSave("echo hi")
	ed.Hooks.OnQuit(false)

	popup := c.VimEditor()
	if popup == nil {
		t.Fatal("popup did not open")
	}
	const msg = "fix the awk filter"
	popupBody := popup.Buffer.String() + "\n" + msg
	if err := popup.Hooks.OnSave(popupBody); err != nil {
		t.Fatalf("popup OnSave: %v", err)
	}
	popup.Hooks.OnQuit(false)

	if len(fs.amendCalls) != 1 {
		t.Fatalf("expected 1 AmendVersionMessage call, got %d", len(fs.amendCalls))
	}
	got := fs.amendCalls[0]
	if got.path != "/bin/scan" {
		t.Errorf("amend path: got %q, want /bin/scan", got.path)
	}
	if got.message != msg {
		t.Errorf("amend message: got %q, want %q", got.message, msg)
	}
}

// TestVimPopup_QuitForceSkipsMessage: :q! in the popup leaves no
// message recorded and returns to ModeInput.
func TestVimPopup_QuitForceSkipsMessage(t *testing.T) {
	fs := newStubPopupFS()
	c := newConfiguredCore(t, fs)

	c.StartVim("/bin/scan")
	ed := c.VimEditor()
	ed.Hooks.OnSave("echo hi")
	ed.Hooks.OnQuit(false)

	popup := c.VimEditor()
	if popup == nil {
		t.Fatal("popup did not open")
	}
	popup.Hooks.OnQuit(true)

	if len(fs.amendCalls) != 0 {
		t.Errorf("after :q! in popup: expected 0 AmendVersionMessage calls, got %d", len(fs.amendCalls))
	}
	if c.Mode() != shellcore.ModeInput {
		t.Errorf("after :q!: expected ModeInput, got %v", c.Mode())
	}
}

// TestVimPopup_EmptyMessageNoRecord: saving a popup buffer that's just
// the comment block produces no AmendVersionMessage call (equivalent
// to skipping).
func TestVimPopup_EmptyMessageNoRecord(t *testing.T) {
	fs := newStubPopupFS()
	c := newConfiguredCore(t, fs)

	c.StartVim("/bin/scan")
	ed := c.VimEditor()
	ed.Hooks.OnSave("echo hi")
	ed.Hooks.OnQuit(false)

	popup := c.VimEditor()
	popupBody := popup.Buffer.String()
	popup.Hooks.OnSave(popupBody)
	popup.Hooks.OnQuit(false)

	if len(fs.amendCalls) != 0 {
		t.Errorf("comment-only buffer: expected 0 AmendVersionMessage calls, got %d", len(fs.amendCalls))
	}
}

// TestVimPopup_WarningStillOpensPopup: a WriteFile that returns a
// non-blocking warning.Warning (e.g. the /cron.d truncation lint) must
// still open the commit-message popup as usual — the save succeeded
// — with the warning folded into the popup header. This is the
// generic mechanism from docs/plans/cron-truncation-lint.md: the
// popup doesn't know or care which check produced the warning.
func TestVimPopup_WarningStillOpensPopup(t *testing.T) {
	fs := newStubPopupFS()
	fs.writeErr = warning.New("tail -n 1000 caps this cron at 1000 rows per run")
	c := newConfiguredCore(t, fs)

	c.StartVim("/cron.d/risky")
	ed := c.VimEditor()
	if err := ed.Hooks.OnSave("# schedule: */5 * * * *\ntail -n 1000 x | grep -c y"); err != nil {
		t.Fatalf("OnSave: got error %v, want nil (a Warning must not block the save)", err)
	}
	ed.Hooks.OnQuit(false)

	if c.Mode() != shellcore.ModeVim {
		t.Fatalf("after :wq with a warning: expected ModeVim (popup), got %v", c.Mode())
	}
	popup := c.VimEditor()
	if popup == nil {
		t.Fatal("popup editor not active")
	}
	body := popup.Buffer.String()
	if !strings.Contains(body, "# Commit message for /cron.d/risky") {
		t.Errorf("popup buffer should still mention the saved path; got:\n%s", body)
	}
	if !strings.Contains(body, "# WARNING: tail -n 1000 caps this cron at 1000 rows per run") {
		t.Errorf("popup buffer should fold in the warning; got:\n%s", body)
	}
}

// TestVimPopup_BlockingErrorSkipsPopup: a real (non-Warning) error
// from WriteFile must behave exactly as before this change — it
// propagates as the blocking error, so vim's `:wq` handling shows it
// as the status-bar message and never calls OnQuit/opens the popup.
func TestVimPopup_BlockingErrorSkipsPopup(t *testing.T) {
	fs := newStubPopupFS()
	fs.writeErr = errors.New("bash syntax error: unexpected EOF")
	c := newConfiguredCore(t, fs)

	c.StartVim("/cron.d/broken")
	ed := c.VimEditor()
	err := ed.Hooks.OnSave("# schedule: */5 * * * *\ncat |")
	if err == nil {
		t.Fatal("OnSave: want the blocking error to propagate, got nil")
	}
	if err.Error() != "bash syntax error: unexpected EOF" {
		t.Errorf("OnSave error: got %q, want the real message unwrapped, not a generic one", err.Error())
	}
}

// TestVimPopup_MixedBlockingAndWarning_StillBlocks: if WriteFile ever
// returns a joined error containing both a real error and a Warning,
// the real error must still win — a Warning is never allowed to mask
// an actual rejection.
func TestVimPopup_MixedBlockingAndWarning_StillBlocks(t *testing.T) {
	fs := newStubPopupFS()
	fs.writeErr = errors.Join(errors.New("watch limit reached"), warning.New("also this"))
	c := newConfiguredCore(t, fs)

	c.StartVim("/cron.d/over-limit")
	ed := c.VimEditor()
	if err := ed.Hooks.OnSave("# schedule: */5 * * * *\ncat /log/x/y/main.log"); err == nil {
		t.Fatal("OnSave: want a blocking error, got nil")
	}
}

func TestIsVersionedPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/bin/scan":                 true,
		"/cron.d/errs":              true,
		"/etc/dashboards/prod.yaml": true,
		"/etc/notify.yaml":          true,
		"/bin/":                     false,
		"/etc/dashboards/":          false,
		"/etc/passwd":               false,
		"/tmp/x":                    false,
	} {
		if got := isVersionedPath(p); got != want {
			t.Errorf("isVersionedPath(%q) = %v, want %v", p, got, want)
		}
	}
}
