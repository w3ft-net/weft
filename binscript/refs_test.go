package binscript

import (
	"fmt"
	"strings"
	"testing"
)

func TestLuaRefs(t *testing.T) {
	src := `
local errs = weft.avg("errors", "1h")
if weft.is_above('disk', 80) then print("hot") end
local function trend() return weft["slope"]("heap", "6h") end
print(weft.latest("holders"), trend())
print(weft.max("pre" .. "fix", "1h"))  -- built at run time: not seen
local w = weft; print(w.sum("aliased", "1h"))  -- through an alias: not seen
`
	got, err := LuaRefs(src)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[errors disk heap]" {
		t.Fatalf("refs = %v, want [errors disk heap]", got)
	}
	if _, err := LuaRefs("print(weft.avg(\"x\""); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("bad Lua: %v", err)
	}
}
