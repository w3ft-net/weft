package store

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
	got, err := luaRefs(src)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[errors disk heap]" {
		t.Fatalf("refs = %v, want [errors disk heap]", got)
	}
	if _, err := luaRefs("print(weft.avg(\"x\""); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("bad Lua: %v", err)
	}
}

// Inline `lua '<program>'` stages are scanned like the bash around them,
// and a program that doesn't parse is refused at save.
func TestCompileWatchPipeline_InlineLua(t *testing.T) {
	got, err := CompileWatchPipeline(`lua 'print(weft.avg("errors", "1h"))' | alert above 5`)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got.NumericRefs) != "[errors]" {
		t.Fatalf("NumericRefs = %v, want [errors]", got.NumericRefs)
	}
	if _, err := CompileWatchPipeline(`lua 'print(weft.avg("errors"'`); err == nil || !strings.Contains(err.Error(), "lua: syntax error") {
		t.Fatalf("inline Lua syntax error: %v", err)
	}
	// A program built at run time can't be read at save.
	if got, err := CompileWatchPipeline(`lua "$prog"`); err != nil || len(got.NumericRefs) != 0 {
		t.Fatalf("dynamic program: refs=%v err=%v", got.NumericRefs, err)
	}
}

func TestSaveShellFunction_TextTypeChecks(t *testing.T) {
	db := testOpen(t)
	org, _ := db.FindOrCreateOrg("bin-types")
	if err := db.SaveCronWatch(org.ID, "", "holders", "every 5m", "# type: text\necho i-0abc", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(org.ID, "", "errors", "every 5m", "echo 1", true, nil); err != nil {
		t.Fatal(err)
	}

	for lang, src := range map[string]string{
		"lua":  `print(weft.avg("holders", "1h"))`,
		"bash": `weft::avg holders 1h`,
	} {
		if err := db.SaveShellFunction(org.ID, "", "avg-"+lang, src, lang); err == nil || !strings.Contains(err.Error(), `cron "holders" stores text`) {
			t.Fatalf("%s /bin reading a text cron: %v", lang, err)
		}
	}
	if err := db.SaveShellFunction(org.ID, "", "inline", `lua 'print(weft.avg("holders", "1h"))'`, "bash"); err == nil || !strings.Contains(err.Error(), "stores text") {
		t.Fatalf("bash /bin with inline Lua reading a text cron: %v", err)
	}
	if err := db.SaveShellFunction(org.ID, "", "who", `print(weft.latest("holders"))`, "lua"); err != nil {
		t.Fatalf("weft.latest of a text cron: %v", err)
	}

	// Turning a cron text is refused while a /bin script reads it.
	if err := db.SaveShellFunction(org.ID, "", "error-avg", `print(weft.avg("errors", "1h"))`, "lua"); err != nil {
		t.Fatal(err)
	}
	err := db.SaveCronWatch(org.ID, "", "errors", "every 5m", "# type: text\necho busy", true, nil)
	if err == nil || !strings.Contains(err.Error(), "/bin/error-avg") {
		t.Fatalf("turning a cron read by /bin text: %v", err)
	}
}
