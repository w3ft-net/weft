package shell

import (
	"strings"
	"testing"

	"github.com/kevlarcon/warp/pkg/shellcore"
)

// stubPopupFS is a minimal in-memory FS that satisfies shellcore.FS
// and exposes an AmendVersionMessage hook for the popup tests.
// Independent of weft's real ShellFS so the tests don't need Postgres.
type stubPopupFS struct {
	files      map[string]string
	amendCalls []amendCall
}

type amendCall struct{ path, message string }

func newStubPopupFS() *stubPopupFS {
	return &stubPopupFS{files: map[string]string{}}
}

func (f *stubPopupFS) ReadFile(p string) ([]byte, error) {
	if v, ok := f.files[p]; ok {
		return []byte(v), nil
	}
	return nil, nil
}

func (f *stubPopupFS) WriteFile(p string, d []byte) error {
	f.files[p] = string(d)
	return nil
}

func (f *stubPopupFS) ReadDir(p string) ([]string, error) { return nil, nil }
func (f *stubPopupFS) Remove(p string) error              { return nil }

func (f *stubPopupFS) AmendVersionMessage(path, message string) error {
	f.amendCalls = append(f.amendCalls, amendCall{path, message})
	return nil
}

// newConfiguredCore builds a ShellCore wired through Shell.configureCore
// with a stub FS, so the popup-flow tests exercise the same code path
// production weft uses.
func newConfiguredCore(t *testing.T, fs shellcore.FS) *shellcore.ShellCore {
	t.Helper()
	core := shellcore.NewShellCore(nil, 120, 40,
		func(p []byte) {},
		func(shellcore.Action) {},
	)
	(&Shell{User: "alice", FS: fs}).configureCore(core)
	return core
}

// TestVimPopup_SaveOnBin_OpensMessagePopup is the regression guard for
// the move that takes the post-save commit-message popup out of warp's
// shellcore (where it was hardcoded as the default OnQuit branch on
// versioned paths) and into weft (where it lives as embedder VimHooks
// installed via configureCore).
//
// Pre-move: passes via shellcore's default isVersionedPath +
// openMessagePopup. Post-move: passes via weft's VimHooks. Same
// observable outcome — :wq on /bin/scan re-enters ModeVim with a
// message-prefixed buffer rather than returning to the prompt.
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
