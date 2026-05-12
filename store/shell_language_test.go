package store

import "testing"

// TestDetectLanguage_ShebangLua is the historical contract: a Lua
// shebang on the first line marks the script as Lua and the shebang
// is stripped from the stored source.
func TestDetectLanguage_ShebangLua(t *testing.T) {
	lang, clean := detectLanguage("severity", "#!/usr/bin/lua\nprint('hi')\n")
	if lang != "lua" {
		t.Errorf("lang: got %q want lua", lang)
	}
	if clean != "print('hi')\n" {
		t.Errorf("clean: got %q want %q", clean, "print('hi')\n")
	}
}

// TestDetectLanguage_ExtensionLuaNoShebang adds the new contract:
// a name ending in .lua is enough to mark the script as Lua, with
// no shebang required. Source is returned unchanged (nothing to
// strip).
func TestDetectLanguage_ExtensionLuaNoShebang(t *testing.T) {
	src := "local x = 1\nprint(x)\n"
	lang, clean := detectLanguage("foo.lua", src)
	if lang != "lua" {
		t.Errorf("lang: got %q want lua (name has .lua extension)", lang)
	}
	if clean != src {
		t.Errorf("clean: got %q want %q (no shebang, source untouched)", clean, src)
	}
}

// TestDetectLanguage_ExtensionLuaWithShebang verifies the extension
// and shebang play together: both agree on Lua, and the shebang is
// still stripped (so users don't end up with a duplicate marker if
// they put a shebang in a .lua-suffixed file).
func TestDetectLanguage_ExtensionLuaWithShebang(t *testing.T) {
	lang, clean := detectLanguage("calc.lua", "#!/usr/bin/lua\nlocal x=1\n")
	if lang != "lua" {
		t.Errorf("lang: got %q want lua", lang)
	}
	if clean != "local x=1\n" {
		t.Errorf("clean: got %q want shebang stripped", clean)
	}
}

// TestDetectLanguage_BashDefault remains the default: no .lua
// extension, no Lua shebang → bash, source unchanged. This is the
// path every existing /bin/ script in the seed takes.
func TestDetectLanguage_BashDefault(t *testing.T) {
	src := "cat /log/foo.log | grep ERROR\n"
	lang, clean := detectLanguage("scan", src)
	if lang != "bash" {
		t.Errorf("lang: got %q want bash", lang)
	}
	if clean != src {
		t.Errorf("clean: got %q want unchanged", clean)
	}
}

// TestDetectLanguage_DotLuaInMiddleNotExtension prevents a false
// positive: only a trailing ".lua" counts, not "lua" appearing
// anywhere in the name. `mylua_helper` is bash.
func TestDetectLanguage_DotLuaInMiddleNotExtension(t *testing.T) {
	lang, _ := detectLanguage("mylua_helper", "echo hi\n")
	if lang != "bash" {
		t.Errorf("lang: got %q want bash (name has lua substring but no .lua suffix)", lang)
	}
}

// TestDetectLanguage_NameWithPath ensures path-prefixed names work
// — the detector should look at the basename's suffix, not the
// full path. Callers historically pass bare names, but the bin
// mount's MountFS rel path could feed a fuller string.
func TestDetectLanguage_NameWithPath(t *testing.T) {
	lang, _ := detectLanguage("/bin/calc.lua", "print(1)\n")
	if lang != "lua" {
		t.Errorf("lang: got %q want lua", lang)
	}
}
