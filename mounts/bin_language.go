package mounts

import "strings"

// ClassifyBinScript decides the script language for a /bin/<name>
// write. Two signals, name first:
//
//   - a `.lua` filename suffix marks the script as Lua regardless of
//     body — file extensions are an authorship convention this layer
//     respects without having to peek at the source.
//   - a leading `#!…lua…` shebang is the secondary signal for files
//     without the suffix (the historical `/bin/severity` shape).
//     The shebang is stripped from the returned cleanSource because
//     gopher-lua's parser rejects shebang-prefixed sources.
//
// Bash is the default for everything else.
//
// Lives in mounts/ (not store/) because the .lua-suffix convention is
// bin-mount domain knowledge: the store just persists bytes plus a
// language tag, and shouldn't have to know which paths are scripts.
// Callers (BinMount.WriteFile, the legacy api/handlers PUT endpoint,
// the coordinator gRPC SaveFunction, the demo seed) classify here and
// pass language to store.SaveShellFunction explicitly.
func ClassifyBinScript(name, source string) (lang, cleanSource string) {
	if strings.HasSuffix(scriptBaseName(name), ".lua") {
		_, rest, hasShebang := stripLuaShebang(source)
		if hasShebang {
			return "lua", rest
		}
		return "lua", source
	}
	if _, rest, ok := stripLuaShebang(source); ok {
		return "lua", rest
	}
	return "bash", source
}

// scriptBaseName returns the trailing path component of name, so the
// suffix check works whether the caller passed "foo.lua" or
// "/bin/foo.lua". Inlined `/`-split rather than importing
// path/filepath to keep ClassifyBinScript allocation-free.
func scriptBaseName(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// stripLuaShebang returns the source with its leading `#!…lua…`
// line removed, and a bool indicating whether such a line was
// present. Returns the original source untouched when no Lua
// shebang is found.
func stripLuaShebang(source string) (firstLine, rest string, hasLuaShebang bool) {
	firstLine, rest, _ = strings.Cut(source, "\n")
	trimmed := strings.TrimSpace(firstLine)
	if strings.HasPrefix(trimmed, "#!") && strings.Contains(trimmed, "lua") {
		return firstLine, rest, true
	}
	return firstLine, source, false
}
