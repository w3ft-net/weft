package shellcore

import (
	"strings"
	"testing"
)

// stubFS is the minimum FS we need for the message-popup tests.
// Implements MessageAmender by recording calls; the underlying file
// content is just stored in a map for ReadFile/WriteFile.
type stubFS struct {
	files       map[string]string
	amendCalls  []amendCall
	amendErr    error
	amendsAtAll bool // false → does NOT implement MessageAmender (test variant)
}

type amendCall struct{ path, message string }

func newStubFS() *stubFS {
	return &stubFS{files: map[string]string{}, amendsAtAll: true}
}

func (f *stubFS) ReadFile(p string) ([]byte, error) {
	if v, ok := f.files[p]; ok {
		return []byte(v), nil
	}
	return nil, nil
}
func (f *stubFS) WriteFile(p string, d []byte) error {
	f.files[p] = string(d)
	return nil
}
func (f *stubFS) ReadDir(p string) ([]string, error) { return nil, nil }
func (f *stubFS) Remove(p string) error              { return nil }

// AmendVersionMessage records the call. Only present on stubFS;
// the "doesn't implement" variant uses a wrapper without this method.
func (f *stubFS) AmendVersionMessage(path, message string) error {
	if f.amendErr != nil {
		return f.amendErr
	}
	f.amendCalls = append(f.amendCalls, amendCall{path, message})
	return nil
}

// newCoreWithStubFS builds a ShellCore with a no-op write callback,
// the given FS, and a fresh state. Width/height set to a generous
// terminal size so the renderer doesn't truncate.
func newCoreWithStubFS(t *testing.T, fs FS) *ShellCore {
	t.Helper()
	c := NewShellCore(nil, 120, 40, func(p []byte) {}, func(Action) {})
	c.SetFS(fs)
	return c
}

// TestVim_SaveOnBin_OpensMessagePopup: :w on /bin/foo followed by :q
// should re-enter ModeVim with a message-prefixed buffer rather than
// returning to the prompt.
func TestVim_SaveOnBin_OpensMessagePopup(t *testing.T) {
	fs := newStubFS()
	c := newCoreWithStubFS(t, fs)

	c.startVim("/bin/scan")
	// Type some content (skip — content already has empty default).
	// Force a save by directly invoking the OnSave hook with new content.
	if err := c.vim.editor.Hooks.OnSave("echo hi"); err != nil {
		t.Fatalf("OnSave: %v", err)
	}
	// Quit triggers the OnQuit hook; this is where the popup logic
	// kicks in for /bin/cron.d paths.
	c.vim.editor.Hooks.OnQuit(false)

	if c.mode != ModeVim {
		t.Fatalf("after :wq on /bin: expected mode=ModeVim (popup), got %v", c.mode)
	}
	if c.vim == nil {
		t.Fatalf("after :wq on /bin: c.vim should be the message-popup editor, got nil")
	}
	// Buffer should open with the comment-prefixed prompt.
	got := c.vim.editor.Buffer.String()
	if !strings.Contains(got, "# Commit message for /bin/scan") {
		t.Errorf("popup buffer should mention the saved path; got:\n%s", got)
	}
	if !strings.Contains(got, "Save & exit") {
		t.Errorf("popup buffer should explain the save/quit semantics; got:\n%s", got)
	}
}

// TestVim_SaveOnTmp_NoPopup: /tmp is not a versioned namespace, so
// saving there should NOT open the message buffer — the user should
// return to the shell prompt as usual.
func TestVim_SaveOnTmp_NoPopup(t *testing.T) {
	fs := newStubFS()
	c := newCoreWithStubFS(t, fs)

	c.startVim("/tmp/notes")
	c.vim.editor.Hooks.OnSave("scratch")
	c.vim.editor.Hooks.OnQuit(false)

	if c.mode != ModeInput {
		t.Errorf("after :wq on /tmp: expected ModeInput, got %v", c.mode)
	}
	if c.vim != nil {
		t.Errorf("after :wq on /tmp: c.vim should be nil (no popup); got %+v", c.vim)
	}
}

// TestVim_SaveOnCronD_OpensPopup: same shape as bin.
func TestVim_SaveOnCronD_OpensPopup(t *testing.T) {
	fs := newStubFS()
	c := newCoreWithStubFS(t, fs)

	c.startVim("/cron.d/errors")
	c.vim.editor.Hooks.OnSave("# schedule: every 1m\necho 1")
	c.vim.editor.Hooks.OnQuit(false)

	if c.mode != ModeVim {
		t.Fatalf("after :wq on /cron.d: expected ModeVim (popup); got %v", c.mode)
	}
	if !strings.Contains(c.vim.editor.Buffer.String(), "# Commit message for /cron.d/errors") {
		t.Errorf("popup buffer should mention path; got:\n%s", c.vim.editor.Buffer.String())
	}
}

// TestVim_MessagePopup_SaveRecordsAmend: typing a message and :wq
// in the popup calls AmendVersionMessage with the stripped content.
func TestVim_MessagePopup_SaveRecordsAmend(t *testing.T) {
	fs := newStubFS()
	c := newCoreWithStubFS(t, fs)

	c.startVim("/bin/scan")
	c.vim.editor.Hooks.OnSave("echo hi")
	c.vim.editor.Hooks.OnQuit(false)
	if c.vim == nil {
		t.Fatal("popup did not open")
	}

	// Now in the popup. User types "fix the awk filter", :wq.
	const msg = "fix the awk filter"
	popupBody := c.vim.editor.Buffer.String() + "\n" + msg
	if err := c.vim.editor.Hooks.OnSave(popupBody); err != nil {
		t.Fatalf("popup OnSave: %v", err)
	}
	c.vim.editor.Hooks.OnQuit(false)

	if len(fs.amendCalls) != 1 {
		t.Fatalf("expected 1 AmendVersionMessage call, got %d", len(fs.amendCalls))
	}
	got := fs.amendCalls[0]
	if got.path != "/bin/scan" {
		t.Errorf("path: got %q, want /bin/scan", got.path)
	}
	if got.message != msg {
		t.Errorf("message: got %q, want %q", got.message, msg)
	}
}

// TestVim_MessagePopup_QuitForceSkips: :q! in the popup leaves no
// message recorded.
func TestVim_MessagePopup_QuitForceSkips(t *testing.T) {
	fs := newStubFS()
	c := newCoreWithStubFS(t, fs)

	c.startVim("/bin/scan")
	c.vim.editor.Hooks.OnSave("echo hi")
	c.vim.editor.Hooks.OnQuit(false)
	// In popup; user types nothing, :q!
	c.vim.editor.Hooks.OnQuit(true)

	if len(fs.amendCalls) != 0 {
		t.Errorf("after :q! in popup: expected 0 AmendVersionMessage calls, got %d", len(fs.amendCalls))
	}
	if c.mode != ModeInput {
		t.Errorf("after :q!: expected ModeInput, got %v", c.mode)
	}
}

// TestVim_MessagePopup_EmptyMessageNoRecord: saving the popup buffer
// with no actual message content (just the comment block) records
// nothing — same as :q!.
func TestVim_MessagePopup_EmptyMessageNoRecord(t *testing.T) {
	fs := newStubFS()
	c := newCoreWithStubFS(t, fs)

	c.startVim("/bin/scan")
	c.vim.editor.Hooks.OnSave("echo hi")
	c.vim.editor.Hooks.OnQuit(false)

	// Popup; user types nothing → buffer is just the comment prefix.
	popupBody := c.vim.editor.Buffer.String()
	c.vim.editor.Hooks.OnSave(popupBody)
	c.vim.editor.Hooks.OnQuit(false)

	if len(fs.amendCalls) != 0 {
		t.Errorf("comment-only buffer: expected 0 AmendVersionMessage calls, got %d", len(fs.amendCalls))
	}
}

// TestVim_NoAmenderInterface_NoCrash: an FS without MessageAmender
// shouldn't crash; the popup just opens and the save is a no-op
// (the message goes nowhere). This is the dev-shell / web-shell
// path.
func TestVim_NoAmenderInterface_NoCrash(t *testing.T) {
	fs := &noAmendStub{files: map[string]string{}}
	c := newCoreWithStubFS(t, fs)

	c.startVim("/bin/scan")
	c.vim.editor.Hooks.OnSave("echo hi")
	c.vim.editor.Hooks.OnQuit(false)

	// Popup still opens (we use ClassifyPath, not the FS).
	if c.mode != ModeVim {
		t.Fatalf("expected popup to open even without AmendVersionMessage; got mode=%v", c.mode)
	}
	c.vim.editor.Hooks.OnSave(c.vim.editor.Buffer.String() + "\nmsg")
	c.vim.editor.Hooks.OnQuit(false)
	// No panic, no record (interface absent).
}

// noAmendStub implements FS without MessageAmender.
type noAmendStub struct{ files map[string]string }

func (f *noAmendStub) ReadFile(p string) ([]byte, error) {
	if v, ok := f.files[p]; ok {
		return []byte(v), nil
	}
	return nil, nil
}
func (f *noAmendStub) WriteFile(p string, d []byte) error { f.files[p] = string(d); return nil }
func (f *noAmendStub) ReadDir(p string) ([]string, error) { return nil, nil }
func (f *noAmendStub) Remove(p string) error              { return nil }
