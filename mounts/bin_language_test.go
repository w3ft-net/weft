package mounts

import "testing"

// TestClassifyBinScript_ShebangLua is the historical contract: a Lua
// shebang on the first line marks the script as Lua and the shebang
// is stripped from the stored source.
func TestClassifyBinScript_ShebangLua(t *testing.T) {
	lang, clean := ClassifyBinScript("severity", "#!/usr/bin/lua\nprint('hi')\n")
	if lang != "lua" {
		t.Errorf("lang: got %q want lua", lang)
	}
	if clean != "print('hi')\n" {
		t.Errorf("clean: got %q want %q", clean, "print('hi')\n")
	}
}

// TestClassifyBinScript_ExtensionLuaNoShebang adds the new contract:
// a name ending in .lua is enough to mark the script as Lua, with
// no shebang required. Source is returned unchanged (nothing to
// strip).
func TestClassifyBinScript_ExtensionLuaNoShebang(t *testing.T) {
	src := "local x = 1\nprint(x)\n"
	lang, clean := ClassifyBinScript("foo.lua", src)
	if lang != "lua" {
		t.Errorf("lang: got %q want lua (name has .lua extension)", lang)
	}
	if clean != src {
		t.Errorf("clean: got %q want %q (no shebang, source untouched)", clean, src)
	}
}

// TestClassifyBinScript_ExtensionLuaWithShebang verifies the extension
// and shebang play together: both agree on Lua, and the shebang is
// still stripped (so users don't end up with a duplicate marker if
// they put a shebang in a .lua-suffixed file).
func TestClassifyBinScript_ExtensionLuaWithShebang(t *testing.T) {
	lang, clean := ClassifyBinScript("calc.lua", "#!/usr/bin/lua\nlocal x=1\n")
	if lang != "lua" {
		t.Errorf("lang: got %q want lua", lang)
	}
	if clean != "local x=1\n" {
		t.Errorf("clean: got %q want shebang stripped", clean)
	}
}

// TestClassifyBinScript_BashDefault remains the default: no .lua
// extension, no Lua shebang → bash, source unchanged. This is the
// path every existing /bin/ script in the seed takes.
func TestClassifyBinScript_BashDefault(t *testing.T) {
	src := "cat /log/foo.log | grep ERROR\n"
	lang, clean := ClassifyBinScript("scan", src)
	if lang != "bash" {
		t.Errorf("lang: got %q want bash", lang)
	}
	if clean != src {
		t.Errorf("clean: got %q want unchanged", clean)
	}
}

// TestClassifyBinScript_DotLuaInMiddleNotExtension prevents a false
// positive: only a trailing ".lua" counts, not "lua" appearing
// anywhere in the name. `mylua_helper` is bash.
func TestClassifyBinScript_DotLuaInMiddleNotExtension(t *testing.T) {
	lang, _ := ClassifyBinScript("mylua_helper", "echo hi\n")
	if lang != "bash" {
		t.Errorf("lang: got %q want bash (name has lua substring but no .lua suffix)", lang)
	}
}

// TestClassifyBinScript_NameWithPath ensures path-prefixed names work
// — the detector should look at the basename's suffix, not the
// full path. Callers historically pass bare names, but the bin
// mount's MountFS rel path could feed a fuller string.
func TestClassifyBinScript_NameWithPath(t *testing.T) {
	lang, _ := ClassifyBinScript("/bin/calc.lua", "print(1)\n")
	if lang != "lua" {
		t.Errorf("lang: got %q want lua", lang)
	}
}
